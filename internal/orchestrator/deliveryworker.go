package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/session"
)

// DefaultDeliveryRetryWindow is how long a delivery keeps retrying a transient
// failure before the run fails. It is sized for a forge outage: long enough to
// ride out most of them, short enough that a pull request which arrives at the
// end of it still describes a branch somebody is waiting for.
const DefaultDeliveryRetryWindow = 6 * time.Hour

// deliveryAttemptTimeout bounds one attempt. Nothing a delivery sends is
// large, so an attempt that runs this long is stuck rather than slow, and a
// later attempt is the better use of the time.
var deliveryAttemptTimeout = 10 * time.Minute

// deliveryBackoff is the wait before the next attempt after failures failed
// attempts: 30 seconds, doubling to a 15 minute cap. The first waits cover a
// blip; the cap keeps a long outage from costing more than four attempts an
// hour.
var deliveryBackoff = func(failures int) time.Duration {
	const (
		first = 30 * time.Second
		limit = 15 * time.Minute
	)
	d := first
	for i := 1; i < failures && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// staleWorkDirAge is how old an entry in the clone work directory must be
// before startup removes it. A job's clone lives there only while its run
// does, so an old one was left by a process that died. Another orchestrator
// can share the directory, so a young one may still be in use.
const staleWorkDirAge = 24 * time.Hour

// deliveryWorker owns every delivery from the moment its run hands it over
// until it reaches an end. Its plans and progress live in the session store
// and its clones in the jobs directory, so it can stop with the process and
// resume in the next one.
//
// The jobs directory holds two kinds of clone. work/ is where every run
// clones its repository; pending/ is where a clone moves when its run hands a
// delivery over. Both sit in one directory so the move is a rename.
type deliveryWorker struct {
	s      *Service
	dir    string
	window time.Duration

	// ctx is the parent of every delivery's context. Service shutdown cancels
	// it, which stops every attempt and leaves the deliveries stored.
	ctx context.Context
	wg  sync.WaitGroup

	mu    sync.Mutex
	owned map[string]*ownedDelivery
}

// ownedDelivery is a delivery this process is driving.
type ownedDelivery struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	// done is closed when this process stops driving the delivery: it ended,
	// or the process is shutting down.
	done chan struct{}
}

func newDeliveryWorker(s *Service, dir string, window time.Duration) (*deliveryWorker, error) {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "kvarn-jobs-*")
		if err != nil {
			return nil, fmt.Errorf("create jobs dir: %w", err)
		}
		dir = tmp
	}
	for _, sub := range []string{"work", "pending"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create jobs dir: %w", err)
		}
	}
	if window <= 0 {
		window = DefaultDeliveryRetryWindow
	}
	return &deliveryWorker{
		s:      s,
		dir:    dir,
		window: window,
		ctx:    s.shutdownCtx,
		owned:  make(map[string]*ownedDelivery),
	}, nil
}

// newCloneDir creates the directory a run clones its repository into.
func (w *deliveryWorker) newCloneDir() (string, error) {
	return os.MkdirTemp(filepath.Join(w.dir, "work"), "kvarn-clone-*")
}

func (w *deliveryWorker) pendingDir(sessionID string) string {
	return filepath.Join(w.dir, "pending", sessionID)
}

// owns reports whether this process is driving a delivery for the session.
func (w *deliveryWorker) owns(sessionID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.owned[sessionID]
	return ok
}

func (w *deliveryWorker) own(sessionID string) *ownedDelivery {
	ctx, cancel := context.WithCancelCause(w.ctx)
	od := &ownedDelivery{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	w.mu.Lock()
	w.owned[sessionID] = od
	w.mu.Unlock()
	return od
}

func (w *deliveryWorker) release(sessionID string, od *ownedDelivery) {
	w.mu.Lock()
	if w.owned[sessionID] == od {
		delete(w.owned, sessionID)
	}
	w.mu.Unlock()
	od.cancel(nil)
	close(od.done)
}

// start takes over the delivery a run prepared and makes the first attempt
// before it returns, so a delivery that works the first time ends exactly as
// it did when the run delivered by itself. It reports whether the delivery
// continues after it returns, in which case the session's final state is
// written by the worker rather than by the run.
func (w *deliveryWorker) start(plan *deliveryPlan, cloneDir string, extras *deliveryExtras, log *slog.Logger) (continues bool) {
	settled := false
	settle := func() {
		if !settled && extras != nil && extras.settle != nil {
			settled = true
			extras.settle()
		}
	}
	defer settle()

	termCtx := context.Background()
	fail := func(err error) bool {
		settle()
		log.Error("failed to deliver", "error", err)
		_ = w.s.sessionMgr.Fail(termCtx, plan.SessionID, err)
		return false
	}

	dest := w.pendingDir(plan.SessionID)
	if err := os.RemoveAll(dest); err != nil {
		return fail(fmt.Errorf("prepare delivery: %w", err))
	}
	if err := os.Rename(cloneDir, dest); err != nil {
		return fail(fmt.Errorf("prepare delivery: keep the clone: %w", err))
	}
	now := time.Now().UTC()
	d := &session.Delivery{
		SessionID:     plan.SessionID,
		NextAttemptAt: now,
		Deadline:      now.Add(w.window),
	}
	if err := w.save(d, plan); err != nil {
		_ = os.RemoveAll(dest)
		return fail(fmt.Errorf("prepare delivery: %w", err))
	}

	od := w.own(plan.SessionID)
	// Until this returns, the run is still registered, so a cancel can still
	// reach the run's context instead of the worker. Only a cancel is carried
	// over: a cost-cap trip on the run must not stop the delivery of work that
	// is already paid for.
	if extras != nil && extras.jobCtx != nil {
		forward := func() {
			if cause := context.Cause(extras.jobCtx); errors.Is(cause, errJobCancelled) {
				od.cancel(cause)
			}
		}
		stop := context.AfterFunc(extras.jobCtx, forward)
		defer stop()
	}
	res, err := w.attempt(od, d, plan, extras, log)
	settle()

	switch res {
	case attemptRetry:
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.loop(od, d, plan, log)
		}()
		return true
	case attemptStopped:
		w.release(plan.SessionID, od)
		return true
	default:
		w.finish(od, d, plan, res, err, log)
		return false
	}
}

// resume takes over the deliveries a previous process left behind. It
// registers every one of them before it returns, so a cancel that arrives
// right after startup finds the delivery it is meant for.
func (w *deliveryWorker) resume() {
	mgr := w.s.sessionMgr
	if mgr == nil {
		return
	}
	ctx := context.Background()
	rows, err := mgr.ListDeliveries(ctx)
	if err != nil {
		slog.Error("could not load stored deliveries", "error", err)
		return
	}

	keep := make(map[string]bool, len(rows))
	for _, d := range rows {
		log := slog.With("session_id", d.SessionID)
		sess, err := mgr.Get(ctx, d.SessionID)
		if err != nil || sess.State.IsTerminal() {
			w.drop(d.SessionID)
			continue
		}
		var plan deliveryPlan
		if err := json.Unmarshal(d.Plan, &plan); err != nil {
			log.Error("stored delivery cannot be read; failing the run", "error", err)
			_ = mgr.Fail(ctx, d.SessionID, fmt.Errorf("deliver: stored delivery cannot be read: %w", err))
			w.drop(d.SessionID)
			continue
		}
		keep[d.SessionID] = true
		log.Info("resuming delivery", "attempts", d.Attempts, "next_attempt_at", d.NextAttemptAt)
		od := w.own(d.SessionID)
		w.wg.Add(1)
		go func(d *session.Delivery) {
			defer w.wg.Done()
			w.loop(od, d, &plan, log.With("project", plan.Project))
		}(d)
	}

	w.sweep(keep)
}

// sweep removes clones nothing will use again: a pending clone without a
// stored delivery, and a work clone old enough that its run cannot still be
// going.
func (w *deliveryWorker) sweep(keep map[string]bool) {
	pending := filepath.Join(w.dir, "pending")
	if entries, err := os.ReadDir(pending); err == nil {
		for _, e := range entries {
			if !keep[e.Name()] {
				_ = os.RemoveAll(filepath.Join(pending, e.Name()))
			}
		}
	}
	work := filepath.Join(w.dir, "work")
	if entries, err := os.ReadDir(work); err == nil {
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleWorkDirAge {
				continue
			}
			_ = os.RemoveAll(filepath.Join(work, e.Name()))
		}
	}
}

// drop removes a stored delivery and its clone.
func (w *deliveryWorker) drop(sessionID string) {
	if err := w.s.sessionMgr.DeleteDelivery(context.Background(), sessionID); err != nil {
		slog.Warn("could not remove stored delivery", "session_id", sessionID, "error", err)
	}
	_ = os.RemoveAll(w.pendingDir(sessionID))
}

// cancel stops a delivery this process drives and waits until its session
// records the cancel. It reports false when the session has no delivery here.
func (w *deliveryWorker) cancel(sessionID string, cause error) bool {
	w.mu.Lock()
	od, ok := w.owned[sessionID]
	w.mu.Unlock()
	if !ok {
		return false
	}
	od.cancel(cause)
	<-od.done
	return true
}

// wait blocks until every delivery goroutine has stopped, or ctx ends.
func (w *deliveryWorker) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// loop drives a delivery that waits for its next attempt until it ends.
func (w *deliveryWorker) loop(od *ownedDelivery, d *session.Delivery, plan *deliveryPlan, log *slog.Logger) {
	for {
		timer := time.NewTimer(time.Until(d.NextAttemptAt))
		select {
		case <-od.ctx.Done():
			timer.Stop()
			if errors.Is(context.Cause(od.ctx), errJobCancelled) {
				w.finish(od, d, plan, attemptCancelled, nil, log)
				return
			}
			w.release(plan.SessionID, od)
			return
		case <-timer.C:
		}

		res, err := w.attempt(od, d, plan, nil, log)
		switch res {
		case attemptRetry:
			continue
		case attemptStopped:
			w.release(plan.SessionID, od)
			return
		default:
			w.finish(od, d, plan, res, err, log)
			return
		}
	}
}

type attemptResult int

const (
	attemptDelivered attemptResult = iota
	attemptFailed
	attemptCancelled
	// attemptStopped is an attempt the shutdown of this process interrupted.
	// The delivery stays stored for the next process.
	attemptStopped
	// attemptRetry is a transient failure with time left for another attempt.
	attemptRetry
)

// attempt makes one attempt. On a transient failure it schedules the next
// attempt and records why on the session.
func (w *deliveryWorker) attempt(
	od *ownedDelivery, d *session.Delivery, plan *deliveryPlan, extras *deliveryExtras, log *slog.Logger,
) (attemptResult, error) {
	actx, cancel := context.WithTimeout(od.ctx, deliveryAttemptTimeout)
	err := w.publishOnce(actx, d, plan, extras, log)
	cancel()

	if err == nil {
		d.Attempts++
		return attemptDelivered, nil
	}
	if od.ctx.Err() != nil {
		if errors.Is(context.Cause(od.ctx), errJobCancelled) {
			return attemptCancelled, nil
		}
		return attemptStopped, nil
	}

	d.Attempts++
	transient, retryAfter := isTransientDelivery(err)
	if !transient {
		return attemptFailed, fmt.Errorf("deliver: %w", err)
	}
	wait := max(deliveryBackoff(d.Attempts), retryAfter)
	next := time.Now().UTC().Add(wait)
	if next.After(d.Deadline) {
		return attemptFailed, fmt.Errorf("deliver: %w (gave up after %s)", err, attemptCount(d.Attempts))
	}

	d.NextAttemptAt = next
	d.LastError = err.Error()
	if saveErr := w.save(d, plan); saveErr != nil {
		return attemptFailed, fmt.Errorf("deliver: %w (and the retry could not be stored: %v)", err, saveErr)
	}
	log.Warn("delivery failed; retrying", "error", err, "attempt", d.Attempts, "wait", wait)
	msg := fmt.Sprintf("Delivery failed (%s); retrying in %s (attempt %d), until %s",
		describeDeliveryError(err), wait.Round(time.Second), d.Attempts+1, d.Deadline.Format("15:04 MST"))
	_ = w.s.sessionMgr.UpdateState(context.Background(), plan.SessionID, session.StateSubmitting, msg)
	return attemptRetry, nil
}

// publishOnce resolves the forge again and publishes what is left of plan.
func (w *deliveryWorker) publishOnce(
	ctx context.Context, d *session.Delivery, plan *deliveryPlan, extras *deliveryExtras, log *slog.Logger,
) error {
	env, err := w.env(ctx, plan)
	if err != nil {
		return err
	}
	save := func() error { return w.save(d, plan) }
	return w.s.publish(ctx, plan, env, extras, save, log)
}

// env resolves what one attempt needs. A project or forge that no longer
// resolves is permanent: the configuration changed, and waiting does not
// change it back.
func (w *deliveryWorker) env(ctx context.Context, plan *deliveryPlan) (*deliveryEnv, error) {
	repoDir := w.pendingDir(plan.SessionID)
	if _, err := os.Stat(repoDir); err != nil {
		return nil, permanentStepError(fmt.Errorf("the prepared commit is no longer on this host: %w", err))
	}
	if w.s.projectStore == nil {
		return nil, permanentStepError(errors.New("no project store"))
	}
	proj, err := w.s.projectStore.Get(ctx, plan.Project)
	if err != nil {
		return nil, permanentStepError(fmt.Errorf("load project %q: %w", plan.Project, err))
	}
	fr, err := w.s.resolveForge(ctx, proj)
	if err != nil {
		return nil, permanentStepError(err)
	}
	if fr.impl == nil {
		return nil, permanentStepError(fmt.Errorf("project %q has no forge to deliver to", plan.Project))
	}
	return &deliveryEnv{proj: proj, forge: fr.impl, creds: fr.creds, cloneURL: fr.cloneURL, repoDir: repoDir}, nil
}

// save stores the delivery with the plan as it is now. It is not cancelled
// with the attempt: progress that a step made has to reach the store even
// when the step was the last thing the attempt did.
func (w *deliveryWorker) save(d *session.Delivery, plan *deliveryPlan) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode delivery: %w", err)
	}
	d.Plan = raw
	if err := w.s.sessionMgr.SaveDelivery(context.Background(), d); err != nil {
		return fmt.Errorf("store delivery: %w", err)
	}
	return nil
}

// finish records how a delivery ended and removes what it kept.
func (w *deliveryWorker) finish(
	od *ownedDelivery, d *session.Delivery, plan *deliveryPlan, res attemptResult, err error, log *slog.Logger,
) {
	ctx := context.Background()
	outcome := "delivered"
	switch res {
	case attemptDelivered:
		if plan.FailAfter != "" {
			_ = w.s.sessionMgr.Fail(ctx, plan.SessionID, errors.New(plan.FailAfter))
		} else {
			log.Info("job completed successfully")
			_ = w.s.sessionMgr.UpdateState(ctx, plan.SessionID, session.StateCompleted, "Completed")
		}
	case attemptCancelled:
		outcome = "cancelled"
		reason := "cancelled"
		if cause := context.Cause(od.ctx); cause != nil {
			reason = cause.Error()
		}
		_ = w.s.sessionMgr.UpdateState(ctx, plan.SessionID, session.StateCancelled, reason)
	default:
		outcome = "failed"
		log.Error("failed to deliver", "error", err, "attempts", d.Attempts)
		_ = w.s.sessionMgr.Fail(ctx, plan.SessionID, err)
	}
	w.s.instruments.RecordDeliveryEnd(ctx, plan.Project, outcome, d.Attempts)
	w.drop(plan.SessionID)
	w.release(plan.SessionID, od)
}

// attemptCount renders a count of attempts for an error message.
func attemptCount(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}
