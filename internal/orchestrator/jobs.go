package orchestrator

import (
	"context"

	"connectrpc.com/connect/v2"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
)

// RetryJob resubmits the request a finished session was created from. It runs
// the original entry point again rather than reviving the row: a session is the
// record of one run, and a retry is a second run whose outcome must be told
// apart from the first. The resubmission therefore meets the same preconditions
// as the original — the project still readable, the key still scoped to it, the
// backlog not full.
//
// The resubmission starts from wherever the original did, which the session
// records: a run submitted against a pull request is retried against that same
// pull request, and a run submitted against a branch is retried against that
// branch.
func (s *Service) RetryJob(ctx context.Context, req *v1.RetryJobRequest) (*v1.RetryJobResponse, error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "sessions not configured")
	}

	sess, err := s.sessionMgr.Get(ctx, req.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	if err := s.authorizeProject(ctx, sess.ProjectName, kvarnv1connect.OrchestratorServiceRetryJobProcedure); err != nil {
		return nil, err
	}

	if !sess.State.IsTerminal() {
		return nil, connect.Errorf(connect.CodeFailedPrecondition,
			"session %s is still %s; cancel it before retrying", sess.ID, sess.State)
	}

	prompt := req.Prompt
	if prompt == "" {
		prompt = sess.Prompt
	}
	if prompt == "" {
		return nil, connect.Errorf(connect.CodeFailedPrecondition,
			"session %s has no prompt to resubmit", sess.ID)
	}

	log := reqid.LoggerFrom(ctx).With("project", sess.ProjectName, "retry_of", sess.ID)

	// A fresh job that got as far as opening a pull request is refused:
	// resubmitting it would open a second one for the same task, which is what
	// the restart rules elsewhere exist to prevent. Continuing that pull request
	// is a different submission, and the caller can make it directly.
	if !sess.Continuation && sess.PRRef != "" {
		return nil, connect.Errorf(connect.CodeFailedPrecondition,
			"session %s already opened pull request %s; start a job against that pull request rather than retrying this one",
			sess.ID, sess.PRRef)
	}

	// The retry starts where the original started, which is the one thing a
	// retry must not re-derive: a continuation resubmitted as a fresh job would
	// open a second pull request beside the one it was meant to revise.
	//
	// No idempotency key is carried over: a retry is an explicit request for a
	// second run of a job that already finished, which is the opposite of what
	// the original key claimed.
	// The inline definition travels with the retry too: a job whose mode was
	// defined by the request cannot be resubmitted by name alone, because that
	// name means nothing without it.
	//
	// The metadata travels with it as well, and for the opposite reason to the
	// idempotency key: where the job came from is as true of the second attempt
	// as of the first, so the caller's record keeping follows its work rather
	// than stopping at the session that failed.
	modeSpec, err := decodeModeSpec(sess.ModeSpecJSON)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	// The added validation steps travel too: they are part of what the job
	// was asked to be, not of the attempt that failed.
	validation, err := decodeValidation(sess.ValidationJSON)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}

	p := startJobParams{
		project:    sess.ProjectName,
		prompt:     prompt,
		mode:       sess.Mode,
		modeSpec:   modeSpec,
		metadata:   sess.Metadata,
		validation: validation,
		procedure:  kvarnv1connect.OrchestratorServiceRetryJobProcedure,
	}
	if sess.Continuation {
		p.prRef = sess.PRRef
	} else {
		p.branch = sess.BaseBranch
	}

	res, err := s.startJob(ctx, p)
	if err != nil {
		return nil, err
	}
	retried := res.session

	log.Info("job retried", "session_id", retried.ID, "mode", retried.Mode)

	return &v1.RetryJobResponse{SessionId: retried.ID}, nil
}

// SetJobPriority reorders a job that is still in the backlog. Past dispatch the
// value orders nothing — the admission queue holds the request built from the
// priority the job was submitted with — so a later state is refused rather than
// silently accepted.
func (s *Service) SetJobPriority(ctx context.Context, req *v1.SetJobPriorityRequest) (*v1.SetJobPriorityResponse, error) {
	if s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, "sessions not configured")
	}

	sess, err := s.sessionMgr.Get(ctx, req.SessionId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	}
	if err := s.authorizeProject(ctx, sess.ProjectName, kvarnv1connect.OrchestratorServiceSetJobPriorityProcedure); err != nil {
		return nil, err
	}

	previous, ok, err := s.sessionMgr.UpdatePendingPriority(ctx, sess.ID, int(req.Priority))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if !ok {
		return nil, connect.Errorf(connect.CodeFailedPrecondition,
			"session %s is %s; only a job still in the backlog can be reordered", sess.ID, sess.State)
	}

	reqid.LoggerFrom(ctx).Info("job reprioritized",
		"session_id", sess.ID, "project", sess.ProjectName, "from", previous, "to", req.Priority)

	// The head of the backlog may have changed; a pass now rather than at the
	// next tick is what makes a promotion take effect when it is asked for.
	s.dispatcher.poke()

	return &v1.SetJobPriorityResponse{PreviousPriority: int32(previous)}, nil
}
