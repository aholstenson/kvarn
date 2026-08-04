package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/session"
)

// RetryJob resubmits the request a finished session was created from. It runs
// the original entry point again rather than reviving the row: a session is the
// record of one run, and a retry is a second run whose outcome must be told
// apart from the first. The resubmission therefore meets the same preconditions
// as the original — the project still readable, the key still scoped to it, the
// backlog not full.
//
// Which entry point depends on what the session left behind. A session with no
// pull request never got that far, so retrying it is a fresh job. A feedback
// run's retry is another feedback run against the same pull request. A fresh
// job that did open a pull request is refused: resubmitting it would open a
// second one for the same task, which is exactly what the restart rules
// elsewhere exist to prevent.
func (s *Service) RetryJob(ctx context.Context, req *connect.Request[v1.RetryJobRequest]) (*connect.Response[v1.RetryJobResponse], error) {
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

	if !sess.State.IsTerminal() {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s is still %s; cancel it before retrying", sess.ID, sess.State))
	}

	prompt := req.Msg.Prompt
	if prompt == "" {
		prompt = sess.Prompt
	}
	if prompt == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s has no prompt to resubmit", sess.ID))
	}

	log := reqid.LoggerFrom(ctx).With("project", sess.ProjectName, "retry_of", sess.ID)

	var retried *session.Session
	switch {
	case sess.PRRef == "":
		// No idempotency key is carried over: a retry is an explicit request for
		// a second run of a job that already finished, which is the opposite of
		// what the original key claimed.
		var res *submissionResult
		res, err = s.startJob(ctx, startJobParams{
			project:   sess.ProjectName,
			prompt:    prompt,
			branch:    sess.BaseBranch,
			mode:      sess.Mode,
			procedure: req.Spec().Procedure,
		})
		if res != nil {
			retried = res.session
		}
	case sess.Mode == coding.ModeFeedback.Name:
		// As above, no idempotency key is carried over: this is a deliberate
		// second run, not a replay of the first.
		var res *submissionResult
		res, err = s.submitFeedback(ctx, submitFeedbackParams{
			project:   sess.ProjectName,
			prRef:     sess.PRRef,
			feedback:  prompt,
			mode:      sess.Mode,
			procedure: req.Spec().Procedure,
		})
		if res != nil {
			retried = res.session
		}
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s already opened pull request %s; continue it with feedback rather than retrying",
				sess.ID, sess.PRRef))
	}
	if err != nil {
		return nil, err
	}

	log.Info("job retried", "session_id", retried.ID, "mode", retried.Mode)

	return connect.NewResponse(&v1.RetryJobResponse{SessionId: retried.ID}), nil
}

// SetJobPriority reorders a job that is still in the backlog. Past dispatch the
// value orders nothing — the admission queue holds the request built from the
// priority the job was submitted with — so a later state is refused rather than
// silently accepted.
func (s *Service) SetJobPriority(ctx context.Context, req *connect.Request[v1.SetJobPriorityRequest]) (*connect.Response[v1.SetJobPriorityResponse], error) {
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

	previous, ok, err := s.sessionMgr.UpdatePendingPriority(ctx, sess.ID, int(req.Msg.Priority))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s is %s; only a job still in the backlog can be reordered", sess.ID, sess.State))
	}

	reqid.LoggerFrom(ctx).Info("job reprioritized",
		"session_id", sess.ID, "project", sess.ProjectName, "from", previous, "to", req.Msg.Priority)

	// The head of the backlog may have changed; a pass now rather than at the
	// next tick is what makes a promotion take effect when it is asked for.
	s.dispatcher.poke()

	return connect.NewResponse(&v1.SetJobPriorityResponse{PreviousPriority: int32(previous)}), nil
}
