package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
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
	choice modeChoice,
	specJSON string,
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
	// Fork pull requests are out of scope for every mode, not only the ones
	// that push. The head branch lives in another repository, so it is not
	// reachable through the project's own clone URL at all — a read-only run
	// would fail at the clone rather than at the push. Working on one needs a
	// refs/pull/<n>/head fetch, which the branch-shaped mirror does not do.
	if isForkPR(pr) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("pull request %s comes from a fork (%s); its head branch is not in this repository", prRef, pr.HeadRepo))
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
		if err := sameSubmission(claimed, p.prompt, choice.name, specJSON, pr.Ref, claimed.PRRef); err != nil {
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

	// Single-flight applies to modes that push, and only to those: two
	// follow-up commits racing on one branch is the problem, whereas two
	// concurrent reviews of one pull request are simply two reviews. A mode
	// that cannot be resolved yet is treated as pushing, because the cheap
	// mistake here is refusing a review and the expensive one is letting two
	// runs fight over a branch.
	modeKnown, modePushes := choice.pushes()
	prFilter := session.SessionFilter{Project: proj.Name, PRRef: pr.Ref}
	if !modeKnown || modePushes {
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
		Mode:            choice.name,
		ModeSpecJSON:    specJSON,
		PRRef:           pr.Ref,
		HeadBranch:      pr.HeadBranch,
		BaseBranch:      pr.BaseBranch,
		ParentSessionID: parentID,
		Continuation:    true,
		KeyID:           callerKeyID(ctx),
		Priority:        jobPriority(proj, choice.name),
		IdempotencyKey:  p.idempotencyKey,
		Metadata:        p.metadata,
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

// isForkPR reports whether the pull request's head branch lives in another
// repository — which puts it out of reach of a clone of the project's own
// repository, and out of reach of a push with an installation token scoped to
// one org.
func isForkPR(pr *forge.PullRequestDetails) bool {
	return pr.HeadRepo == "" || pr.HeadRepo != pr.BaseRepo
}
