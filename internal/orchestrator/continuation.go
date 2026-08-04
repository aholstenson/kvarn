package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/session"
)

// startContinuation admits a submission that starts from an existing pull
// request. The agent runs against the PR's head branch with the prompt as its
// task and pushes a follow-up commit to that same branch — no second PR is
// opened.
//
// Every rejection happens before a session is created, so a refused request
// leaves no trace.
func (s *Service) startContinuation(
	ctx context.Context,
	p startJobParams,
	proj *project.Project,
	mode *coding.Mode,
	log *slog.Logger,
) (*submissionResult, error) {
	prRef := p.prRef
	log = log.With("pr_ref", prRef)
	log.Info("continuing pull request")

	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		log.Error("failed to resolve forge", "forge", proj.Forge, "error", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if fr.impl == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("project %q has no forge configured; continuing a pull request needs one to read and update it", p.project))
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

	if err := s.checkBacklogDepth(ctx, proj.Name); err != nil {
		return nil, err
	}

	// Serialize the single-flight check with session creation so two requests
	// arriving together cannot both pass it.
	s.continuationMu.Lock()
	defer s.continuationMu.Unlock()

	// The key is resolved inside the lock and before the single-flight check,
	// because the two would otherwise disagree about a retry: the run the first
	// request started is still active, so the check would refuse the retry with
	// "already running" instead of handing back the session it is asking about.
	replay := func(claimed *session.Session) (*submissionResult, error) {
		if err := sameSubmission(claimed, p.prompt, mode.ModeName(), pr.Ref, claimed.PRRef); err != nil {
			return nil, err
		}
		log.Info("idempotent replay", "session_id", claimed.ID)
		return &submissionResult{session: claimed, duplicate: true}, nil
	}
	if p.idempotencyKey != "" {
		claimed, err := s.findClaimedSession(ctx, proj.Name, p.idempotencyKey)
		if err != nil {
			return nil, err
		}
		if claimed != nil {
			return replay(claimed)
		}
	}

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
	var parentID string
	if prior, err := s.sessionMgr.List(ctx, prFilter); err != nil {
		log.Warn("failed to look up prior sessions for pull request", "error", err)
	} else if len(prior) > 0 {
		// Listing is newest-first, so the last row is the original run.
		parentID = prior[len(prior)-1].ID
	}

	// The session's prompt is what the requester actually asked for — both
	// because GetSession should report that, and because it is the input the
	// context pack is rebuilt from at dispatch. Assembling the pack here would
	// freeze a diff that the pull request may outgrow while the run waits.
	sess, err := s.sessionMgr.Create(ctx, session.CreateParams{
		ProjectName:     proj.Name,
		Prompt:          p.prompt,
		Mode:            mode.ModeName(),
		PRRef:           pr.Ref,
		HeadBranch:      pr.HeadBranch,
		BaseBranch:      pr.BaseBranch,
		ParentSessionID: parentID,
		Continuation:    true,
		KeyID:           callerKeyID(ctx),
		Priority:        jobPriority(proj, mode.ModeName()),
		IdempotencyKey:  p.idempotencyKey,
	})
	// continuationMu serializes this against another request in the same process, so
	// a conflict here means a second orchestrator sharing the store claimed the
	// key. It resolves the same way: answer with the session that won.
	if errors.Is(err, session.ErrIdempotencyConflict) {
		claimed, findErr := s.findClaimedSession(ctx, proj.Name, p.idempotencyKey)
		if findErr != nil {
			return nil, findErr
		}
		if claimed == nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("idempotency key claimed by a session that cannot be read back"))
		}
		return replay(claimed)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}

	// Publish the PR up front so watchers can link to it from the first event.
	if err := s.sessionMgr.SetPullRequest(ctx, sess.ID, pr.URL, pr.Ref, pr.HeadBranch); err != nil {
		log.Warn("failed to record pull request on session", "session_id", sess.ID, "error", err)
	}

	log.Info("continuation queued", "session_id", sess.ID, "head_branch", pr.HeadBranch, "parent_session_id", parentID)
	s.dispatcher.poke()

	return &submissionResult{session: sess}, nil
}

// buildFeedbackPrompt assembles the task message for a continuation: what the
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
