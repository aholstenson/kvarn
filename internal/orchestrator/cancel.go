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
	cause := context.Cause(rootCtx)
	switch {
	case errors.Is(cause, errJobRequeued):
		// A drain stopped this run so it could be started again later. The
		// requeue is conditional on the state the run actually unwound from, so
		// one that reached running in the meantime falls through: it has spent
		// against its cost cap, and re-running it would spend twice.
		if s.requeueRun(termCtx, sessionID, cause.Error()) {
			return
		}
		s.sessionMgr.UpdateState(termCtx, sessionID, session.StateCancelled, cause.Error())
	case errors.Is(cause, errJobCancelled):
		s.sessionMgr.UpdateState(termCtx, sessionID, session.StateCancelled, cause.Error())
	default:
		s.sessionMgr.Fail(termCtx, sessionID, err)
	}
}

// cancelCause builds the cause recorded on a cancelled run. The sentinel is
// what failRun matches on, so a reason must wrap it rather than replace it.
func cancelCause(reason string) error {
	if reason == "" {
		return errJobCancelled
	}
	return fmt.Errorf("%w: %s", errJobCancelled, reason)
}

// cancelSession stops one run, whichever tier it is in. The caller has already
// authorized the session's project.
func (s *Service) cancelSession(ctx context.Context, sess *session.Session, cause error, reason string) error {
	if sess.State.IsTerminal() {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s has already finished (%s)", sess.ID, sess.State))
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
			return connect.NewError(connect.CodeInternal, err)
		}
		if cancelled {
			reqid.LoggerFrom(ctx).Info("queued job cancelled",
				"session_id", sess.ID, "project", sess.ProjectName, "reason", reason)
			return nil
		}
	}

	s.runningMu.Lock()
	job, ok := s.running[sess.ID]
	s.runningMu.Unlock()
	if !ok {
		// Non-terminal with no job behind it: the run belonged to a process that
		// is gone. Startup reconciliation settles such sessions, so this is only
		// reachable when a terminal write failed outright.
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s is not running on this orchestrator", sess.ID))
	}

	job.cancel(cause)

	reqid.LoggerFrom(ctx).Info("job cancelled",
		"session_id", sess.ID, "project", sess.ProjectName, "state", sess.State, "reason", reason)
	return nil
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

	if err := s.cancelSession(ctx, sess, cancelCause(req.Msg.Reason), req.Msg.Reason); err != nil {
		return nil, err
	}

	return connect.NewResponse(&v1.CancelJobResponse{PreviousState: string(sess.State)}), nil
}

// defaultCancelJobsLimit bounds one bulk cancel when the caller names no limit.
// A sweep is an operator action taken against a queue whose size they have just
// read, so this is a guard against a runaway command rather than a paging
// mechanism: the response says what it cancelled, and running it again cancels
// the next batch.
const defaultCancelJobsLimit = 100

// CancelJobs stops every job matching a filter. Per-job failures are reported
// on their entry rather than failing the call, because the common one — a job
// finishing between the listing and the cancel — is the sweep working as
// intended, not an error the caller can act on.
func (s *Service) CancelJobs(ctx context.Context, req *connect.Request[v1.CancelJobsRequest]) (*connect.Response[v1.CancelJobsResponse], error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("sessions not configured"))
	}
	msg := req.Msg

	states, err := parseStates(msg.States)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	for _, st := range states {
		if st.IsTerminal() {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("state %q is terminal and has nothing to cancel", st))
		}
	}

	// An unfiltered request cancels everything the caller can reach, which is a
	// legitimate thing to want and a bad thing to do by accident.
	if msg.Project == "" && len(states) == 0 && msg.Mode == "" && msg.PrRef == "" && !msg.All {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("cancelling every active job requires the all flag"))
	}

	// A sweep that names no project reaches every job the caller can see. For a
	// key scoped to named projects that is still only its own work, so it claims
	// nothing new. For a wildcard key it is every job on the host — authority
	// over the orchestrator rather than over a project, which is exactly what
	// the host capability is for. The all flag is a separate question: it guards
	// against an abbreviated command line, not against a caller's reach.
	if msg.Project == "" && s.callerIsWildcard(ctx) {
		if err := s.authorizeHost(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
	}

	limit := int(msg.Limit)
	if limit <= 0 {
		limit = defaultCancelJobsLimit
	}

	// ActiveOnly rather than an explicit non-terminal state list: it is derived
	// from session.TerminalStates, so a new state is swept without being added
	// here.
	candidates, err := s.sessionMgr.List(ctx, session.SessionFilter{
		Project:    msg.Project,
		PRRef:      msg.PrRef,
		Mode:       msg.Mode,
		States:     states,
		ActiveOnly: true,
		Limit:      limit,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	cause := cancelCause(msg.Reason)
	jobs := make([]*v1.CancelledJob, 0, len(candidates))
	for _, sess := range candidates {
		// Sessions the key does not cover are skipped rather than refused: the
		// filter describes a set, and a bulk operation acts on the part of it
		// the caller owns.
		if err := s.authorizeProject(ctx, sess.ProjectName, req.Spec().Procedure); err != nil {
			continue
		}
		entry := &v1.CancelledJob{
			SessionId:     sess.ID,
			Project:       sess.ProjectName,
			PreviousState: string(sess.State),
		}
		if !msg.DryRun {
			if err := s.cancelSession(ctx, sess, cause, msg.Reason); err != nil {
				entry.Error = err.Error()
			}
		}
		jobs = append(jobs, entry)
	}

	reqid.LoggerFrom(ctx).Info("bulk cancel",
		"project", msg.Project, "matched", len(jobs), "dry_run", msg.DryRun, "reason", msg.Reason)

	return connect.NewResponse(&v1.CancelJobsResponse{Jobs: jobs}), nil
}
