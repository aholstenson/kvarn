package orchestrator

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/session"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Draining is how a host is taken out of service without killing what it is
// already doing. It stops the dispatcher from moving work out of the backlog,
// so the pipeline runs down to empty on its own and the process can then be
// stopped with nothing in flight to lose.
//
// It is deliberately a *state* rather than a shutdown: reversible, reported by
// GetQueueStats, and surviving as long as the process does. A drain started for
// a deploy that gets called off is undone with one call, and an operator who
// finds a host doing nothing can see that this is why.
//
// What draining does not do is refuse submissions. The backlog is durable and
// its entries cost a row, so a job accepted onto a draining host waits and then
// runs — here after a resume, or on whichever host picks it up after a restart.
// Refusing would discard work the host is perfectly able to hold, and turn a
// rolling restart into an outage for every client submitting during it.

// drainStatus is the admission stance and why it was set.
type drainStatus struct {
	draining bool
	reason   string
	since    time.Time
}

// errJobRequeued is the cause attached to a run stopped so it can go back to
// the backlog. Like errJobCancelled it is what failRun matches on, and it must
// be told apart from a cancel: a cancelled job is finished, a requeued one has
// merely lost the host it was starting on.
var errJobRequeued = errors.New("job requeued")

// requeueCause builds the cause recorded on a requeued run, wrapping the
// sentinel so a reason cannot replace it.
func requeueCause(reason string) error {
	if reason == "" {
		return errJobRequeued
	}
	return errors.Join(errJobRequeued, errors.New(reason))
}

// isDraining reports whether the dispatcher should stand down.
func (s *Service) isDraining() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drain.draining
}

// drainState returns a copy of the current stance for reporting.
func (s *Service) drainState() drainStatus {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drain
}

// setDrain records the new stance and reports the one it replaced.
func (s *Service) setDrain(draining bool, reason string) drainStatus {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	previous := s.drain
	if draining {
		// Re-draining an already-draining host keeps the original timestamp:
		// "since" is when the host stopped taking work, and a second call does
		// not restart that.
		since := previous.since
		if !previous.draining {
			since = time.Now().UTC()
		}
		s.drain = drainStatus{draining: true, reason: reason, since: since}
	} else {
		s.drain = drainStatus{}
	}
	return previous
}

// SetDrain changes whether the orchestrator dispatches new work.
//
// Draining also returns to the backlog every job that has not started running
// yet. Those jobs have done nothing but read — a clone, a wait for capacity, a
// VM boot — so re-running them reaches the same place, and sending them back is
// what lets a host reach empty in the time its genuine work takes rather than
// its whole pipeline's. The split is `session.RestartableStates`, the same one
// startup reconciliation uses, because it answers the same question: what can
// this host stop doing without destroying anything.
func (s *Service) SetDrain(ctx context.Context, req *connect.Request[v1.SetDrainRequest]) (*connect.Response[v1.SetDrainResponse], error) {
	if err := s.authorizeHost(ctx, req.Spec().Procedure); err != nil {
		return nil, err
	}

	previous := s.setDrain(req.Msg.Draining, req.Msg.Reason)
	log := reqid.LoggerFrom(ctx)

	// A preview cannot be requeued the way a job can: it is a VM in this
	// process, reachable only from inside it, with no backlog row to fall back
	// to. Draining therefore stops previews outright and refuses new boots —
	// the record survives, so the next request on whichever host is serving
	// boots it there.
	s.previews.SetDraining(ctx, req.Msg.Draining)

	var requeued []string
	if req.Msg.Draining {
		requeued = s.requeueUnstarted(ctx, req.Msg.Reason)
		log.Info("orchestrator draining",
			"reason", req.Msg.Reason, "requeued", len(requeued), "dispatched", s.dispatchedCount())
	} else {
		// Resuming has to wake the dispatcher: every pass while drained was a
		// no-op, so without this the backlog waits for the next tick.
		s.dispatcher.poke()
		log.Info("orchestrator resumed", "backlog_waiting", s.dispatchedCount())
	}

	return connect.NewResponse(&v1.SetDrainResponse{
		PreviouslyDraining: previous.draining,
		Requeued:           requeued,
		Dispatched:         int32(s.dispatchedCount()),
	}), nil
}

// requeueUnstarted signals every run that has not started running yet to go
// back to the backlog, returning the sessions it signalled.
//
// The decision here is only a filter — the authoritative check is the
// conditional write in failRun, which runs once the job has actually unwound
// and so sees the state it ended up in rather than the one it had when the
// drain looked. A job that advances from setup to running in between is
// therefore not requeued, and is not silently re-run against a cost cap it has
// already spent from.
func (s *Service) requeueUnstarted(ctx context.Context, reason string) []string {
	if s.sessionMgr == nil {
		return nil
	}

	s.runningMu.Lock()
	ids := make([]string, 0, len(s.running))
	jobs := make([]runningJob, 0, len(s.running))
	for id, job := range s.running {
		ids = append(ids, id)
		jobs = append(jobs, job)
	}
	s.runningMu.Unlock()

	cause := requeueCause(reason)
	var signalled []string
	for i, id := range ids {
		sess, err := s.sessionMgr.Get(ctx, id)
		if err != nil {
			continue
		}
		if !sess.State.IsRestartable() {
			continue
		}
		jobs[i].cancel(cause)
		signalled = append(signalled, id)
	}
	return signalled
}

// requeueRun returns a stopped run to the backlog. It reports whether the run
// went back; false means it had advanced past the point where re-running it is
// free, and the caller records the stop instead.
func (s *Service) requeueRun(ctx context.Context, sessionID, message string) bool {
	ok, err := s.sessionMgr.RequeueRun(ctx, sessionID, session.RequeueOpts{
		Message:     message,
		MaxAttempts: s.dispatcher.maxAttempts(),
	})
	if err != nil {
		reqid.LoggerFrom(ctx).Error("could not requeue run", "session_id", sessionID, "error", err)
		return false
	}
	return ok
}

// drainStatsInto fills the drain fields on a queue stats response.
func (s *Service) drainStatsInto(out *v1.GetQueueStatsResponse) {
	st := s.drainState()
	out.Draining = st.draining
	out.DrainReason = st.reason
	if !st.since.IsZero() {
		out.DrainingSince = timestamppb.New(st.since)
	}
}
