package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/session"
)

// errJobCancelled is the cause attached to a job context stopped by CancelJob.
// The cause is what tells the run's failure paths apart from one another: a
// shutdown cancels with a plain context.Canceled and a tripped budget with
// cost.ErrBudgetExceeded, and both stay failures, while this one is recorded as
// session.StateCancelled.
var errJobCancelled = errors.New("job cancelled")

// runningJob is one entry in the in-flight census: what to cancel, and which
// project to attribute the pipeline slot to.
type runningJob struct {
	cancel  context.CancelCauseFunc
	project string
}

// beginJob creates the root context for a run and registers its cancel func so
// CancelJob can reach it. It is called synchronously by the dispatcher, before
// the goroutine is spawned: creating the context inside runJob would leave a
// window where a claimed job could not yet be cancelled. The caller must pair
// it with endJob.
func (s *Service) beginJob(sessionID, projectName string) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(s.shutdownCtx)
	s.runningMu.Lock()
	s.running[sessionID] = runningJob{cancel: cancel, project: projectName}
	s.runningMu.Unlock()
	return ctx, cancel
}

// endJob deregisters a finished run and wakes the dispatcher, since the slot it
// held is now free. It runs after the run has written its terminal state, so a
// cancel racing the end of a job either finds the job (and cancels a context
// nobody is waiting on any more) or sees the terminal state.
func (s *Service) endJob(sessionID string) {
	s.runningMu.Lock()
	delete(s.running, sessionID)
	s.runningMu.Unlock()
	s.dispatcher.poke()
}

// failRun records the terminal state for a run that did not reach its end. A run
// stopped through CancelJob is recorded as cancelled with the requester's
// reason; anything else is a failure carrying err. Every failure path in runJob
// goes through here, so wherever the cancellation happens to land — a clone, the
// queue, the agent, validation — it is reported the same way.
func (s *Service) failRun(termCtx, rootCtx context.Context, sessionID string, err error) {
	if cause := context.Cause(rootCtx); errors.Is(cause, errJobCancelled) {
		s.sessionMgr.UpdateState(termCtx, sessionID, session.StateCancelled, cause.Error())
		return
	}
	s.sessionMgr.Fail(termCtx, sessionID, err)
}

// CancelJob stops an in-flight run. The job's context is cancelled, which
// unwinds whatever it is doing — waiting in the scheduler queue, cloning,
// running the agent — and its teardown tears the VM down on the way out. The
// session lands in the cancelled state; the write is done by the job itself, so
// this returns as soon as the stop is signalled rather than waiting for the VM.
func (s *Service) CancelJob(ctx context.Context, req *connect.Request[v1.CancelJobRequest]) (*connect.Response[v1.CancelJobResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}

	sess, err := s.sessionMgr.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err := s.authorizeProject(ctx, sess.ProjectName, req.Spec().Procedure); err != nil {
		return nil, err
	}

	if sess.State.IsTerminal() {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s has already finished (%s)", sess.ID, sess.State))
	}

	cause := errJobCancelled
	if reason := req.Msg.Reason; reason != "" {
		cause = fmt.Errorf("%w: %s", errJobCancelled, reason)
	}

	// A job still in the backlog has no context to cancel, so it is stopped
	// where it lives: the same conditional move out of pending that the
	// dispatcher uses to claim it. Whoever's move lands first wins, and a lost
	// race falls through to the running path below — by then the dispatcher has
	// registered the run, so the cancel finds it there.
	if sess.State == session.StatePending {
		cancelled, err := s.sessionMgr.TransitionPending(ctx, sess.ID, session.PendingTransition{
			State:   session.StateCancelled,
			Message: cause.Error(),
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if cancelled {
			reqid.LoggerFrom(ctx).Info("queued job cancelled",
				"session_id", sess.ID, "project", sess.ProjectName, "reason", req.Msg.Reason)
			return connect.NewResponse(&v1.CancelJobResponse{PreviousState: string(sess.State)}), nil
		}
	}

	s.runningMu.Lock()
	job, ok := s.running[sess.ID]
	s.runningMu.Unlock()
	if !ok {
		// Non-terminal with no job behind it: the run belonged to a process that
		// is gone. Startup reconciliation settles such sessions, so this is only
		// reachable when a terminal write failed outright.
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s is not running on this orchestrator", sess.ID))
	}

	job.cancel(cause)

	reqid.LoggerFrom(ctx).Info("job cancelled",
		"session_id", sess.ID, "project", sess.ProjectName, "state", sess.State, "reason", req.Msg.Reason)

	return connect.NewResponse(&v1.CancelJobResponse{PreviousState: string(sess.State)}), nil
}
