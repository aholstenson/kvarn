package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/session"
)

// SubmitFeedback continues work on an existing pull request. The agent runs
// against the PR's head branch with the feedback as its task and pushes a
// follow-up commit to that same branch — no second PR is opened.
//
// Every rejection happens before a session is created, so a refused request
// leaves no trace.
func (s *Service) SubmitFeedback(ctx context.Context, req *connect.Request[v1.SubmitFeedbackRequest]) (*connect.Response[v1.SubmitFeedbackResponse], error) {
	msg := req.Msg

	if err := s.authorizeProject(ctx, msg.Project, req.Spec().Procedure); err != nil {
		return nil, err
	}

	if s.projectStore == nil || s.sessionMgr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("project-aware jobs not configured"))
	}

	prRef := strings.TrimSpace(msg.PrRef)
	if prRef == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("pr_ref is required"))
	}
	if strings.TrimSpace(msg.Feedback) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("feedback is required"))
	}

	modeName := msg.Mode
	if modeName == "" {
		modeName = coding.ModeFeedback.Name
	}
	mode, err := coding.ModeByName(modeName)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	log := reqid.LoggerFrom(ctx).With("project", msg.Project, "pr_ref", prRef, "mode", mode.ModeName())
	log.Info("submitting feedback")

	proj, err := s.projectStore.Get(ctx, msg.Project)
	if err != nil {
		log.Error("project not found", "error", err)
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("project %q: %w", msg.Project, err))
	}

	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		log.Error("failed to resolve forge", "forge", proj.Forge, "error", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if fr.impl == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("project %q has no forge configured; feedback runs need one to read and update the pull request", msg.Project))
	}

	getOpts := forge.GetPROpts{RepoURL: fr.cloneURL, PRRef: prRef, Credentials: fr.creds}
	pr, err := fr.impl.GetPullRequest(ctx, getOpts)
	if err != nil {
		// A ref the forge cannot interpret, or one that does not resolve to a
		// pull request, is a bad request rather than a server fault.
		log.Error("failed to read pull request", "error", err)
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("pull request %q: %w", prRef, err))
	}
	if pr.State != "open" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("pull request %s is %s; only open pull requests can be continued", prRef, pr.State))
	}
	// Pushing to a head branch in another repository requires the
	// maintainer-edit flag and is impossible with an installation token scoped
	// to a single org, so fork PRs are out of scope.
	if pr.HeadRepo == "" || pr.HeadRepo != pr.BaseRepo {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("pull request %s comes from a fork (%s); its head branch cannot be pushed to", prRef, pr.HeadRepo))
	}

	// Diff is context, not a requirement: a run without it is still useful.
	diff, err := fr.impl.GetPullRequestDiff(ctx, getOpts)
	if err != nil {
		log.Warn("failed to fetch pull request diff; continuing without it", "error", err)
		diff = ""
	}

	// Serialize the single-flight check with session creation so two requests
	// arriving together cannot both pass it.
	s.feedbackMu.Lock()
	defer s.feedbackMu.Unlock()

	prFilter := session.SessionFilter{Project: proj.Name, PRRef: pr.Ref}
	activeFilter := prFilter
	activeFilter.ActiveOnly = true
	active, err := s.sessionMgr.List(ctx, activeFilter)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check running sessions: %w", err))
	}
	if len(active) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("session %s is already running for pull request %s", active[0].ID, pr.Ref))
	}

	// Lineage, best-effort: the oldest session on this PR carries the task the
	// pull request came from. Retention may have pruned it.
	var parentID, originalPrompt string
	if prior, err := s.sessionMgr.List(ctx, prFilter); err != nil {
		log.Warn("failed to look up prior sessions for pull request", "error", err)
	} else if len(prior) > 0 {
		// Listing is newest-first, so the last row is the original run.
		oldest := prior[len(prior)-1]
		parentID = oldest.ID
		originalPrompt = oldest.Prompt
	}

	agentPrompt := buildFeedbackPrompt(originalPrompt, pr, diff, msg.Feedback)

	// The session's own prompt stays the raw feedback so GetSession reports
	// what was actually asked, not the assembled context pack.
	sess, err := s.sessionMgr.Create(ctx, session.CreateParams{
		ProjectName:     proj.Name,
		Prompt:          msg.Feedback,
		Mode:            mode.ModeName(),
		PRRef:           pr.Ref,
		HeadBranch:      pr.HeadBranch,
		BaseBranch:      pr.BaseBranch,
		ParentSessionID: parentID,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}

	// Publish the PR up front so watchers can link to it from the first event.
	if err := s.sessionMgr.SetPullRequest(ctx, sess.ID, pr.URL, pr.Ref, pr.HeadBranch); err != nil {
		log.Warn("failed to record pull request on session", "session_id", sess.ID, "error", err)
	}

	log.Info("session created", "session_id", sess.ID, "head_branch", pr.HeadBranch, "parent_session_id", parentID)

	reqID, _ := reqid.FromContext(ctx)
	s.instruments.RecordJobStart(ctx, proj.Name, mode.ModeName())
	s.jobsWG.Add(1)
	rootCtx, cancelJob := s.beginJob(sess.ID)
	go s.runJob(rootCtx, cancelJob, jobSpec{
		requestID:   reqID,
		sessionID:   sess.ID,
		keyID:       callerKeyID(ctx),
		proj:        proj,
		mode:        mode,
		agentPrompt: agentPrompt,
		userPrompt:  msg.Feedback,
		baseBranch:  pr.BaseBranch,
		pr: &prTarget{
			ref:        pr.Ref,
			headBranch: pr.HeadBranch,
			headSHA:    pr.HeadSHA,
			url:        pr.URL,
		},
	})

	return connect.NewResponse(&v1.SubmitFeedbackResponse{SessionId: sess.ID}), nil
}

// buildFeedbackPrompt assembles the task message for a feedback run: what the
// pull request set out to do, what it currently contains, and the feedback to
// act on. Sections whose content is unavailable are left out rather than
// emitted empty — originalTask is missing when retention has pruned the
// originating session, and diff when the forge call failed.
func buildFeedbackPrompt(originalTask string, pr *forge.PullRequestDetails, diff, feedback string) string {
	var sb strings.Builder

	if t := strings.TrimSpace(originalTask); t != "" {
		sb.WriteString("## Original task\n\n")
		sb.WriteString(t)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## Current pull request\n\n")
	fmt.Fprintf(&sb, "%s\n", strings.TrimSpace(pr.Title))
	if body := strings.TrimSpace(pr.Body); body != "" {
		sb.WriteString("\n")
		sb.WriteString(body)
		sb.WriteString("\n")
	}

	if d := strings.TrimSpace(diff); d != "" {
		sb.WriteString("\n## Diff\n\n```diff\n")
		sb.WriteString(d)
		sb.WriteString("\n```\n")
	}

	sb.WriteString("\n## Feedback to address\n\n")
	sb.WriteString(strings.TrimSpace(feedback))

	return sb.String()
}
