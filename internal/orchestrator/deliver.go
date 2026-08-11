package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/session"
)

// deliveryRequest is everything the delivery step needs from a finished run.
type deliveryRequest struct {
	mode        *coding.Mode
	sessionID   string
	sandbox     Sandbox
	forgeImpl   forge.Forge
	proj        *project.Project
	agentResult *agent.Result
	// baseBranch is the branch a new pull request targets.
	baseBranch string
	cloneURL   string
	cloneDir   string
	creds      scm.CredentialSource
	// pr is the pull request the run was started against, or nil for a fresh
	// run.
	pr *prTarget
	// userPrompt is what the requester asked for, quoted back in the comment a
	// delivery posts.
	userPrompt string
	worklog    []worklogEntry
	// valResult is the last validation pass, reported alongside the result so a
	// comment says which steps ran and how they went. Nil when the mode skips
	// validation or the project declares no steps.
	valResult *sandbox.ValidationResult
	// validationFailed marks a run that a `require` mode failed on a red step.
	// The verdict is still delivered — reporting it is what such a mode is for —
	// but the changes are not: a commit sink is skipped, because the run has
	// just established that what it produced does not pass.
	validationFailed bool
	cost             cost.Report
	// behavior is the resolved forge behavior for this run: branch prefix,
	// labels, commit author, and what the pull request and its comments say.
	behavior forgeconfig.Behavior
	log      *slog.Logger
}

// deliverySinkOrder is the order sinks are attempted in, which is not the order
// a mode happens to list them: a comment on the pull request a run just opened
// has to come after the sink that opened it.
var deliverySinkOrder = []coding.Sink{coding.SinkNewPullRequest, coding.SinkFollowUpCommit, coding.SinkPRComment}

// deliver sends the run's output to each of the mode's sinks. A failure stops
// there and is returned, which fails the run: for a mode whose only output is a
// comment, work that never left the host is not a success, and the same rule
// already applies to a pull request that was never opened.
func (s *Service) deliver(ctx context.Context, req deliveryRequest) error {
	if req.mode.DeliversTo(coding.SinkNone) || len(req.mode.Deliver) == 0 {
		req.log.Info("nothing to deliver: mode delivers to none")
		return nil
	}
	if req.forgeImpl == nil {
		req.log.Info("skipping delivery: no forge configured")
		return nil
	}
	// A validation result is worth a comment on its own: a mode whose output is
	// a verdict on someone else's branch has something to say even when the
	// agent's last turn said nothing.
	if req.agentResult == nil && req.valResult == nil {
		req.log.Info("skipping delivery: no agent result")
		return nil
	}

	// Every sink needs a forge API token, so resolve once to decide whether
	// delivery is possible at all. The sinks below pass the source on rather
	// than this value: by the time each one pushes or calls the API it
	// re-resolves, which is what keeps a job longer than the credential's
	// lifetime from failing.
	submitCreds, err := scm.Resolve(ctx, req.creds)
	if err != nil {
		return fmt.Errorf("resolve credentials for submission: %w", err)
	}
	if submitCreds.APIToken() == "" {
		req.log.Info("skipping delivery: no token in credentials")
		return nil
	}

	// A commit-producing sink posts its own comment carrying the summary and
	// the work log, so an explicit pr-comment alongside one would put nearly
	// the same text on the pull request twice.
	commented := false

	for _, sink := range deliverySinkOrder {
		if !req.mode.DeliversTo(sink) {
			continue
		}
		// A run that failed a required step under `require` has nothing worth
		// landing, and one with no agent result has nothing to title a commit
		// with. The comment sink below still fires in both cases, which is the
		// whole point: the verdict is the output.
		if sink == coding.SinkNewPullRequest || sink == coding.SinkFollowUpCommit {
			if req.validationFailed {
				req.log.Info("skipping commit delivery: required validation failed", "sink", sink)
				continue
			}
			if req.agentResult == nil {
				req.log.Info("skipping commit delivery: no agent result", "sink", sink)
				continue
			}
		}
		// Started against a pull request, the changes belong on it. Opening a
		// second one would target the first one's head branch, which is not what
		// naming a pull request asked for. The two commit sinks are alternatives
		// a mode cannot declare together, so nothing else fires as a result.
		if sink == coding.SinkNewPullRequest && req.pr != nil {
			req.log.Info("committing onto the pull request the run started from rather than opening a new one",
				"pr_ref", req.pr.ref)
			sink = coding.SinkFollowUpCommit
		}

		switch sink {
		case coding.SinkNewPullRequest:
			if err := s.submitChanges(ctx, req.sessionID, req.sandbox, req.forgeImpl, req.agentResult,
				req.proj, req.behavior, req.mode.Name, req.baseBranch, req.cloneURL, req.cloneDir, req.creds,
				req.userPrompt, req.worklog, req.cost, req.log); err != nil {
				return err
			}
			commented = true

		case coding.SinkFollowUpCommit:
			if req.pr == nil {
				return fmt.Errorf("mode %q delivers a follow-up commit but the run has no pull request to commit onto", req.mode.Name)
			}
			if err := s.submitFollowup(ctx, req.sessionID, req.sandbox, req.forgeImpl, req.agentResult,
				req.proj, req.behavior, req.mode.Name, req.pr, req.cloneURL, req.cloneDir, req.creds,
				req.userPrompt, req.worklog, req.cost, req.log); err != nil {
				return err
			}
			commented = true

		case coding.SinkPRComment:
			if commented {
				req.log.Info("skipping result comment: the commit already posted one")
				continue
			}
			if err := s.postResultComment(ctx, req); err != nil {
				return err
			}
		}
	}
	return nil
}

// postResultComment puts the agent's written result on the pull request. It is
// the delivery a read-only mode uses: a review of somebody else's pull request
// is worth nothing if it only reaches the session event log.
//
// The pull request it comments on is the one the run started from, or the one
// an earlier sink opened — which is why the session is re-read rather than
// assumed.
func (s *Service) postResultComment(ctx context.Context, req deliveryRequest) error {
	prRef := ""
	if req.pr != nil {
		prRef = req.pr.ref
	} else if sess, err := s.sessionMgr.Get(ctx, req.sessionID); err == nil {
		prRef = sess.PRRef
	}
	if prRef == "" {
		return fmt.Errorf("mode %q delivers a comment but the run has no pull request to comment on", req.mode.Name)
	}

	s.sessionMgr.UpdateState(ctx, req.sessionID, session.StateSubmitting, "Posting result comment")

	result := ""
	if req.agentResult != nil {
		result = req.agentResult.Description
	}
	body := formatResultComment(req.userPrompt, result, req.valResult,
		req.worklog, sectionsFrom(req.behavior.PullRequest), req.cost)
	if body == "" {
		// The comment is this mode's entire output, so there is nothing left to
		// deliver — but an empty comment says even less than none.
		req.log.Info("skipping result comment: the run produced nothing to say")
		return nil
	}
	if err := req.forgeImpl.PostComment(ctx, forge.PostCommentOpts{
		RepoURL:     req.cloneURL,
		PRRef:       prRef,
		Body:        body,
		Credentials: req.creds,
	}); err != nil {
		return fmt.Errorf("post result comment on %s: %w", prRef, err)
	}
	req.log.Info("result comment posted", "pr_ref", prRef)
	return nil
}

// maxCommentBody is GitHub's limit on a comment body. A run that produced more
// than this has its comment trimmed rather than rejected: a truncated review is
// worth more than an API error.
const maxCommentBody = 65536

// truncationNote replaces what a trimmed comment body dropped.
const truncationNote = "\n\n_(truncated: the full result is available with `kvarn jobs result`)_"

// formatResultComment renders what the run produced as a pull request comment:
// the agent's written result, how the project's validation steps went, the task
// it was given, and the same collapsible work log and cost sections every other
// comment carries.
func formatResultComment(prompt, result string, val *sandbox.ValidationResult,
	entries []worklogEntry, sections commentSections, report cost.Report,
) string {
	var sb strings.Builder
	writeSection(&sb, "Result", result)
	writeValidation(&sb, val)
	writeQuotedRequest(&sb, "Task", prompt, sections.quote)
	writeWorklog(&sb, sections.worklog, entries)
	writeCostSection(&sb, sections.cost, report)
	return trimCommentBody(finishComment(&sb))
}

// writeValidation appends what the project's validation steps did, or nothing
// when none ran. A mode whose output is a verdict on someone else's branch is
// only as useful as this section: without it the comment reads the same whether
// the suite passed or failed.
func writeValidation(sb *strings.Builder, val *sandbox.ValidationResult) {
	if val == nil || (len(val.Required) == 0 && len(val.Advisory) == 0) {
		return
	}
	sb.WriteString("\n\n## Validation\n\n")
	if val.RequiredPassed {
		sb.WriteString("All required steps passed.\n\n")
	} else {
		sb.WriteString("**A required step failed.**\n\n")
	}
	writeStepLines(sb, "Required", val.Required)
	writeStepLines(sb, "Advisory", val.Advisory)
}

// writeStepLines renders one group of steps as a bullet per step.
func writeStepLines(sb *strings.Builder, heading string, steps []sandbox.StepResult) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintf(sb, "%s:\n\n", heading)
	for _, step := range steps {
		fmt.Fprintf(sb, "- %s — %s\n", step.Name, describeStep(step))
	}
	sb.WriteString("\n")
}

// describeStep is one step's outcome in the words a reader wants: whether it
// ran at all, and if it did, whether it passed.
func describeStep(step sandbox.StepResult) string {
	switch {
	case step.Skipped:
		return "skipped (no changed files matched its paths)"
	case step.Err != nil:
		return fmt.Sprintf("could not run: %v", step.Err)
	case step.ExitCode != 0:
		return fmt.Sprintf("failed (exit %d)", step.ExitCode)
	default:
		return "passed"
	}
}

// trimCommentBody cuts a comment body to what the forge will accept, on a rune
// boundary so the result stays valid UTF-8.
func trimCommentBody(body string) string {
	if len(body) <= maxCommentBody {
		return body
	}
	cut := maxCommentBody - len(truncationNote)
	for cut > 0 && !utf8RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + truncationNote
}

// utf8RuneStart reports whether b begins a UTF-8 encoded rune.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
