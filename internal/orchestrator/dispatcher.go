package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/session"
)

// The job pipeline has two tiers, and the dispatcher is what joins them.
//
// Tier one is the durable backlog: a submission is written to the session store
// and its RPC returns. It holds no clone, no goroutine and no resolved
// footprint — just the request — so it costs a row, survives a restart, and can
// be bounded orders of magnitude higher than anything held in memory.
//
// Tier two is the in-memory pipeline every job used to enter directly: a
// goroutine that clones, reads kvarn.yml to learn its footprint, and waits in
// the admission queue for capacity. That is genuinely expensive per waiter —
// each one holds a clone on the same filesystem the disk pool is sized from —
// so its population stays bounded at MaxDispatched.
//
// The split exists because a job's footprint cannot be known before its clone,
// which is what forces admission so late in the run. Rather than fight that,
// the backlog sits in front of it and answers a cheaper question: which
// requests deserve to pay the clone cost next.

// DispatchPolicy configures the dispatcher. The zero value dispatches
// everything immediately with no backlog bound, which is what the tests and any
// caller that has not configured a scheduler want.
type DispatchPolicy struct {
	// MaxDispatched bounds how many jobs may occupy the in-memory pipeline at
	// once — cloning, queued for capacity, or running. Zero is unbounded.
	MaxDispatched int
	// PriorityAgeStep is how much waiting earns a backlog entry one level of
	// effective priority. It mirrors the admission queue's own aging so a job
	// is ordered by the same rule on both sides of dispatch.
	PriorityAgeStep time.Duration
	// MaxBacklog bounds the durable backlog. Zero is unbounded.
	MaxBacklog int
	// MaxQueueWait fails a backlog entry that has waited longer than this.
	// Without it a host that was down over a weekend boots into a flood of work
	// nobody is waiting for any more. Zero never expires.
	MaxQueueWait time.Duration
	// MaxAttempts caps how many dispatches one job may spend. It is the same
	// bound startup reconciliation applies, held here as well because a drain
	// requeues live runs and must not push a job past it. Zero is no cap.
	MaxAttempts int
	// Interval is the backstop tick. The dispatcher is woken by submissions and
	// by jobs finishing, so this only has to catch what neither signals: an
	// entry aging into expiry, or a wake lost to a full channel.
	Interval time.Duration
}

// defaultDispatchInterval is the backstop tick when none is configured.
const defaultDispatchInterval = 30 * time.Second

// dispatchCandidates bounds how many backlog entries one pass considers. A pass
// can under-fill when the head of the backlog belongs to projects already at
// their share of the pipeline; the next pass re-queries, so the cost of the
// bound is a little latency rather than a stalled job.
const dispatchCandidates = 256

// dispatcher moves work from the durable backlog into the in-memory pipeline.
type dispatcher struct {
	svc    *Service
	policy DispatchPolicy
	wake   chan struct{}
}

func newDispatcher(svc *Service, policy DispatchPolicy) *dispatcher {
	if policy.Interval <= 0 {
		policy.Interval = defaultDispatchInterval
	}
	return &dispatcher{
		svc:    svc,
		policy: policy,
		// Buffered by one: a wake that arrives while a pass is already pending
		// is redundant, since that pass will see the new row.
		wake: make(chan struct{}, 1),
	}
}

// backlogBound is the configured backlog limit, nil-safe so a Service assembled
// without a dispatcher reads as unbounded rather than panicking.
func (d *dispatcher) backlogBound() int {
	if d == nil {
		return 0
	}
	return d.policy.MaxBacklog
}

// maxDispatched is the configured pipeline bound, nil-safe like backlogBound.
func (d *dispatcher) maxDispatched() int {
	if d == nil {
		return 0
	}
	return d.policy.MaxDispatched
}

// maxAttempts is the configured dispatch cap, nil-safe like backlogBound.
func (d *dispatcher) maxAttempts() int {
	if d == nil {
		return 0
	}
	return d.policy.MaxAttempts
}

// ageStep is how much waiting earns a backlog entry one level of effective
// priority. Callers that report an entry's place need the same value the
// dispatcher orders by, so they read it from here rather than re-deriving it
// from config.
func (d *dispatcher) ageStep() time.Duration {
	if d == nil {
		return 0
	}
	return d.policy.PriorityAgeStep
}

// poke asks for a dispatch pass. It never blocks.
func (d *dispatcher) poke() {
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// start runs the dispatch loop until ctx is cancelled.
func (d *dispatcher) start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(d.policy.Interval)
		defer ticker.Stop()
		for {
			d.pass(ctx)
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
			case <-ticker.C:
			}
		}
	}()
}

// pass expires stale entries, then fills whatever room the pipeline has.
func (d *dispatcher) pass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	// Without both stores there is no backlog to read and no project to resolve
	// a job against. StartJob refuses such a service with Unimplemented, so
	// nothing can legitimately be waiting; standing down keeps a bridge-only or
	// VM-only configuration from tripping over sessions a test placed directly.
	if d.svc.sessionMgr == nil || d.svc.projectStore == nil {
		return
	}
	// A draining host stands down entirely, expiry included. The backlog is
	// what a drain exists to preserve, and failing an entry for a wait the
	// operator imposed would be the drain destroying the work it is protecting.
	// Both resume together.
	//
	// The check bounds what a drain stops, not what is already in progress: a
	// pass that began before the drain was set runs to its end. That costs at
	// most one more job started and one more expiry sweep of entries already
	// past their deadline, both of which the drain was a moment too late for
	// rather than wrong about.
	if d.svc.isDraining() {
		return
	}
	d.expire(ctx)

	free := d.freeSlots()
	if free <= 0 {
		return
	}

	candidates, err := d.svc.sessionMgr.ListPending(ctx, session.PendingQuery{
		Now:     time.Now(),
		AgeStep: d.policy.PriorityAgeStep,
		Limit:   dispatchCandidates,
	})
	if err != nil {
		slog.Error("could not read job backlog", "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}

	perProject := d.perProjectCap(candidates)
	held := d.svc.dispatchedPerProject()

	for _, sess := range candidates {
		if free <= 0 {
			return
		}
		if perProject > 0 && held[sess.ProjectName] >= perProject {
			continue
		}
		if !d.dispatch(ctx, sess) {
			continue
		}
		held[sess.ProjectName]++
		free--
	}
}

// freeSlots is how many more jobs the in-memory pipeline will take.
func (d *dispatcher) freeSlots() int {
	if d.policy.MaxDispatched <= 0 {
		return dispatchCandidates
	}
	free := d.policy.MaxDispatched - d.svc.dispatchedCount()
	if free < 0 {
		return 0
	}
	return free
}

// perProjectCap is each project's share of the pipeline: the bound divided by
// how many projects currently want it, counting both what is already dispatched
// and what is waiting. One project alone is capped at the whole pipeline, which
// is right — nobody else is asking for it — and a second project appearing
// halves the first one's share, so a burst cannot lock anyone out.
//
// The share is recomputed each pass and nothing already dispatched is taken
// back, so it converges as jobs finish rather than applying at once. Zero means
// no cap, which is what an unbounded pipeline gets.
func (d *dispatcher) perProjectCap(candidates []*session.Session) int {
	if d.policy.MaxDispatched <= 0 {
		return 0
	}
	projects := make(map[string]struct{}, len(candidates))
	for _, sess := range candidates {
		projects[sess.ProjectName] = struct{}{}
	}
	for name := range d.svc.dispatchedPerProject() {
		projects[name] = struct{}{}
	}
	if len(projects) <= 1 {
		return d.policy.MaxDispatched
	}
	cap := d.policy.MaxDispatched / len(projects)
	if cap < 1 {
		cap = 1
	}
	return cap
}

// dispatch claims one backlog entry and hands it to a run. It reports whether
// the entry was claimed — a false return means somebody else took it (a cancel,
// or a competing pass) and the caller should move on without spending a slot.
func (d *dispatcher) dispatch(ctx context.Context, sess *session.Session) bool {
	// Claiming before anything else is what settles the race with CancelJob:
	// both sides attempt the same conditional move out of pending and exactly
	// one wins. Everything after this point owns the session.
	claimed, err := d.svc.sessionMgr.TransitionPending(ctx, sess.ID, session.PendingTransition{
		State:   session.StateQueued,
		Message: "Dispatched; preparing to clone",
	})
	if err != nil {
		slog.Error("could not claim backlog entry", "session_id", sess.ID, "error", err)
		return false
	}
	if !claimed {
		return false
	}

	log := slog.With("session_id", sess.ID, "project", sess.ProjectName, "mode", sess.Mode)
	if waited := time.Since(sess.QueuedAt); waited > time.Second {
		log.Info("dispatched from backlog", "queue_wait", waited.String(), "attempt", sess.Attempts+1)
	}

	d.svc.instruments.RecordJobStart(ctx, sess.ProjectName, sess.Mode)
	d.svc.jobsWG.Add(1)
	rootCtx, cancelJob := d.svc.beginJob(sess.ID, sess.ProjectName)
	go d.svc.startClaimed(rootCtx, cancelJob, sess)
	return true
}

// expire fails backlog entries that have waited past MaxQueueWait.
func (d *dispatcher) expire(ctx context.Context) {
	if d.policy.MaxQueueWait <= 0 {
		return
	}
	cutoff := time.Now().Add(-d.policy.MaxQueueWait)
	reason := fmt.Sprintf("queued longer than %s without being dispatched", d.policy.MaxQueueWait)
	ids, err := d.svc.sessionMgr.ExpirePending(ctx, cutoff, reason)
	if err != nil {
		slog.Error("could not expire stale backlog entries", "error", err)
		return
	}
	if len(ids) > 0 {
		slog.Warn("expired stale backlog entries", "count", len(ids), "max_queue_wait", d.policy.MaxQueueWait)
	}
}

// startClaimed resolves a claimed backlog entry into a runnable spec and runs
// it. Resolution happens here rather than at submission because a backlog entry
// may have waited: the project, its forge credentials and — for a feedback run
// — the pull request and its diff are all read as they are now, not as they
// were when the request arrived.
func (s *Service) startClaimed(rootCtx context.Context, cancelJob context.CancelCauseFunc, sess *session.Session) {
	spec, err := s.resolveSpec(rootCtx, sess)
	if err != nil {
		slog.Error("could not prepare dispatched job", "session_id", sess.ID, "project", sess.ProjectName, "error", err)
		s.failRun(context.WithoutCancel(rootCtx), rootCtx, sess.ID, err)
		s.endJob(sess.ID)
		cancelJob(nil)
		s.jobsWG.Done()
		return
	}
	s.runJob(rootCtx, cancelJob, spec)
}

// resolveSpec rebuilds everything a run needs from the persisted request. The
// backlog stores what was asked for, never what it resolved to, so a project
// deleted or a key revoked while the job waited stops it here rather than
// running it against configuration nobody holds any more.
func (s *Service) resolveSpec(ctx context.Context, sess *session.Session) (jobSpec, error) {
	mode, err := coding.ModeByName(sess.Mode)
	if err != nil {
		return jobSpec{}, err
	}

	proj, err := s.projectStore.Get(ctx, sess.ProjectName)
	if err != nil {
		return jobSpec{}, fmt.Errorf("project %q: %w", sess.ProjectName, err)
	}

	if err := s.recheckKey(ctx, sess); err != nil {
		return jobSpec{}, err
	}

	spec := jobSpec{
		sessionID:   sess.ID,
		keyID:       sess.KeyID,
		proj:        proj,
		mode:        mode,
		agentPrompt: sess.Prompt,
		userPrompt:  sess.Prompt,
		baseBranch:  sess.BaseBranch,
	}

	if !sess.Continuation {
		return spec, nil
	}
	return s.resolveContinuationSpec(ctx, sess, proj, spec)
}

// recheckKey re-validates the submitting API key at dispatch. The key's scopes
// were checked when the request arrived, but a backlog entry can outlive them,
// and work authorized by a key an operator has since revoked should not run.
func (s *Service) recheckKey(ctx context.Context, sess *session.Session) error {
	if !s.authEnabled || sess.KeyID == "" || s.apiKeyStore == nil {
		return nil
	}
	key, err := s.apiKeyStore.Get(ctx, sess.KeyID)
	if err != nil {
		return fmt.Errorf("api key %q is no longer valid: %w", sess.KeyID, err)
	}
	if key.Expired(time.Now()) {
		return fmt.Errorf("api key %q expired while the job was queued", key.Name)
	}
	if !key.AllowsProject(sess.ProjectName) {
		return fmt.Errorf("api key %q is no longer allowed for project %q", key.Name, sess.ProjectName)
	}
	return nil
}

// resolveContinuationSpec assembles a continuation's context pack against the
// pull request's current state. Submission rejected the cases that can be known
// up front; they are re-checked here because a pull request can be merged,
// closed or moved while its run waits in the backlog.
func (s *Service) resolveContinuationSpec(ctx context.Context, sess *session.Session, proj *project.Project, spec jobSpec) (jobSpec, error) {
	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		return jobSpec{}, fmt.Errorf("resolve forge: %w", err)
	}
	if fr.impl == nil {
		return jobSpec{}, fmt.Errorf("project %q has no forge configured", sess.ProjectName)
	}

	getOpts := forge.GetPROpts{RepoURL: fr.cloneURL, PRRef: sess.PRRef, Credentials: fr.creds}
	pr, err := fr.impl.GetPullRequest(ctx, getOpts)
	if err != nil {
		return jobSpec{}, fmt.Errorf("read pull request %q: %w", sess.PRRef, err)
	}
	if pr.State != "open" {
		return jobSpec{}, fmt.Errorf("pull request %s is %s; only open pull requests can be continued", sess.PRRef, pr.State)
	}
	if pr.HeadRepo == "" || pr.HeadRepo != pr.BaseRepo {
		return jobSpec{}, fmt.Errorf("pull request %s comes from a fork (%s); its head branch cannot be pushed to", sess.PRRef, pr.HeadRepo)
	}

	// Diff is context, not a requirement: a run without it is still useful.
	diff, err := fr.impl.GetPullRequestDiff(ctx, getOpts)
	if err != nil {
		slog.Warn("could not fetch pull request diff; continuing without it",
			"session_id", sess.ID, "pr_ref", sess.PRRef, "error", err)
		diff = ""
	}

	// Lineage is best-effort: retention may have pruned the run the pull
	// request came from, in which case the pack simply omits the original task.
	originalPrompt := ""
	if sess.ParentSessionID != "" {
		if parent, err := s.sessionMgr.Get(ctx, sess.ParentSessionID); err != nil {
			slog.Debug("parent session unavailable for feedback context",
				"session_id", sess.ID, "parent_session_id", sess.ParentSessionID, "error", err)
		} else {
			originalPrompt = parent.Prompt
		}
	}

	spec.agentPrompt = buildFeedbackPrompt(originalPrompt, pr, diff, sess.Prompt)
	spec.baseBranch = pr.BaseBranch
	spec.pr = &prTarget{
		ref:        pr.Ref,
		headBranch: pr.HeadBranch,
		headSHA:    pr.HeadSHA,
		url:        pr.URL,
	}
	return spec, nil
}

// errBacklogFull is returned to a submission the backlog cannot take.
var errBacklogFull = errors.New("job backlog is full; retry when the host has caught up")
