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
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/session"
)

// deliveryRequest is everything the delivery step needs from a finished run.
type deliveryRequest struct {
	mode      *coding.Mode
	sessionID string
	sandbox   Sandbox
	forgeImpl forge.Forge
	proj      *project.Project
	// cfg is the branch's kvarn.yml, read here for the preview sites a comment
	// can link to. Nil when the checkout declares no configuration.
	cfg         *projconfig.Config
	agentResult *agent.Result
	// baseBranch is the branch a new pull request targets.
	baseBranch string
	cloneURL   string
	cloneDir   string
	creds      scm.CredentialSource
	// pr is the pull request the run was started against, or nil for a fresh
	// run.
	pr *prTarget
	// reviser revises the pull request's title and description after a
	// follow-up commit. Nil when the run's agent cannot.
	reviser agent.PullRequestReviser
	// userPrompt is what the requester asked for, quoted back in the comment a
	// delivery posts.
	userPrompt string
	// metadata is the submission's annotations, available here so an operator's
	// comment header can name them.
	metadata map[string]string
	worklog  []worklogEntry
	// checkpoints is how many commits the run already recorded in the clone — a
	// merge, today. A run that made one has something to push even when its
	// trailing diff is empty.
	checkpoints int
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

// prepareDelivery turns the run's output into a delivery plan for the mode's
// sinks: it takes the changes out of the VM, commits them in the clone, and
// writes down everything the publish steps will send. It returns nil when there
// is nothing to deliver.
//
// Only the publish steps talk to the forge. Everything here is local, so an
// error here fails the run at once, the same as any other failure inside it,
// while a failure to reach the forge is something the delivery can wait out.
func (s *Service) prepareDelivery(ctx context.Context, req deliveryRequest) (*deliveryPlan, *deliveryExtras, error) {
	if req.mode.DeliversTo(coding.SinkNone) || len(req.mode.Deliver) == 0 {
		req.log.Info("nothing to deliver: mode delivers to none")
		return nil, nil, nil
	}
	if req.forgeImpl == nil {
		req.log.Info("skipping delivery: no forge configured")
		return nil, nil, nil
	}
	// A validation result is worth a comment on its own: a mode whose output is
	// a verdict on someone else's branch has something to say even when the
	// agent's last turn said nothing.
	if req.agentResult == nil && req.valResult == nil {
		req.log.Info("skipping delivery: no agent result")
		return nil, nil, nil
	}

	// Every sink needs a forge API token, so resolve once to decide whether
	// delivery is possible at all. The publish steps resolve the credentials
	// again on every attempt, which is what keeps a delivery longer than the
	// credential's lifetime from failing.
	submitCreds, err := scm.Resolve(ctx, req.creds)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve credentials for submission: %w", err)
	}
	if submitCreds.APIToken() == "" {
		req.log.Info("skipping delivery: no token in credentials")
		return nil, nil, nil
	}

	plan := &deliveryPlan{SessionID: req.sessionID, Project: req.proj.Name}
	extras := &deliveryExtras{}

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
			if err := s.prepareNewPullRequest(ctx, req, plan); err != nil {
				return nil, nil, err
			}
			commented = true

		case coding.SinkFollowUpCommit:
			if req.pr == nil {
				return nil, nil, fmt.Errorf("mode %q delivers a follow-up commit but the run has no pull request to commit onto", req.mode.Name)
			}
			if err := s.prepareFollowup(ctx, req, plan, extras); err != nil {
				return nil, nil, err
			}
			commented = true

		case coding.SinkPRComment:
			if commented {
				req.log.Info("skipping result comment: the commit already posted one")
				continue
			}
			if err := s.prepareResultComment(ctx, req, plan); err != nil {
				return nil, nil, err
			}
		}
	}
	if plan.Push == nil && plan.Comment == nil {
		return nil, nil, nil
	}
	return plan, extras, nil
}

// prepareNewPullRequest commits the run's changes on a new branch and plans
// the push, the pull request and the comment that goes with it.
func (s *Service) prepareNewPullRequest(ctx context.Context, req deliveryRequest, plan *deliveryPlan) error {
	log := req.log
	// Check if there are any changes. A commit the run already recorded counts:
	// a run that only merged its base branch has an empty trailing diff and a
	// merge commit that must still reach the forge.
	changedFiles, err := req.sandbox.ChangedFiles(ctx)
	if err != nil {
		return fmt.Errorf("check changed files for submission: %w", err)
	}
	if len(changedFiles) == 0 && req.checkpoints == 0 {
		log.Info("no changes to submit")
		return nil
	}

	title := req.agentResult.Title
	if title == "" {
		title = "Apply agent changes"
	}
	body := req.agentResult.Description

	s.sessionMgr.UpdateState(ctx, req.sessionID, session.StateSubmitting, "Submitting changes")

	// Extract changes from VM to host clone dir.
	if err := req.sandbox.ExtractChanges(ctx, req.cloneDir); err != nil {
		return fmt.Errorf("extract changes from VM: %w", err)
	}

	behavior := req.behavior
	// The commit message and PR body share one body so the PR shows the same
	// content that lands as the merge commit. Only the run metadata differs:
	// trailers on the commit, where tooling looks for them, and a footer on the
	// pull request, where a reader does.
	//
	// The branch name is derived from the commit title for readability, with a
	// short slice of the session id as a suffix to keep it unique and git-ref
	// safe. If the title yields no usable slug, fall back to the session id.
	prBranch := fmt.Sprintf("%s/%s", behavior.BranchPrefix, req.sessionID)
	if slug := branchSlug(title); slug != "" {
		suffix := req.sessionID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		prBranch = fmt.Sprintf("%s/%s-%s", behavior.BranchPrefix, slug, suffix)
	}
	commitMsg := title
	if body != "" {
		commitMsg = title + "\n\n" + body
	}

	tmplData := prTemplateData{
		Title:       title,
		Description: body,
		SessionID:   req.sessionID,
		Branch:      prBranch,
		Mode:        req.mode.Name,
		Metadata:    req.metadata,
	}
	sites := s.previewSitesFor(req.proj, req.cfg)
	// The pull request does not exist yet, so only the sites this branch is
	// addressed by on its own can be named here. A site addressed by `{pr}`
	// resolves for the comment, which is rendered once there is a pull request
	// to name.
	withLinks := tmplData.withPreviewLinks(sites.links(prBranch, ""))
	// Trailers render strictly because they land in the commit message, where a
	// key the submission did not carry would leave a trailer with nothing after
	// the colon in history nobody will rewrite.
	commitMsg = coding.ApplyTrailers(commitMsg,
		renderPRTemplates(behavior.PullRequest.CommitTrailers, withLinks, missingKeyError, log))
	prBody := coding.ApplyFooter(body,
		renderPRTemplate(behavior.PullRequest.BodyFooter, withLinks, missingKeyZero, log))

	sha, err := req.forgeImpl.SCM().Commit(ctx, scm.CommitOpts{
		RepoDir:     req.cloneDir,
		Branch:      prBranch,
		Message:     commitMsg,
		AuthorName:  behavior.CommitAuthorName,
		AuthorEmail: behavior.CommitAuthorEmail,
	})
	if err != nil {
		return fmt.Errorf("commit to %s: %w", prBranch, err)
	}

	plan.Push = &pushStep{Branch: prBranch, Commit: sha}
	plan.PullRequest = &pullRequestStep{
		BaseBranch:  req.baseBranch,
		Title:       title,
		Body:        prBody,
		Description: body,
		Labels:      behavior.Labels,
	}
	// The comment carries the task and the work log, so they stay out of any
	// squash-merge commit message.
	plan.Comment = &commentStep{
		Kind: commentNewPullRequest,
		Input: &commentInput{
			Request:  req.userPrompt,
			Worklog:  toWorklogRecords(req.worklog),
			Cost:     req.cost,
			Sections: commentSectionSpecFrom(behavior.PullRequest, forgeconfig.CommentNewPullRequest),
			Data:     tmplData,
			Preview:  sites,
		},
	}
	return nil
}

// prepareFollowup commits the run's changes onto the pull request's head
// branch and plans the push and the comment that says what was addressed. No
// new pull request is opened.
func (s *Service) prepareFollowup(ctx context.Context, req deliveryRequest, plan *deliveryPlan, extras *deliveryExtras) error {
	log := req.log
	pr := req.pr
	// The VM cloned the PR head, so the changed set is the follow-up delta. A
	// merge-only run has none and a merge commit waiting in the clone, so a
	// commit already recorded counts as something to submit.
	changedFiles, err := req.sandbox.ChangedFiles(ctx)
	if err != nil {
		return fmt.Errorf("check changed files for submission: %w", err)
	}
	if len(changedFiles) == 0 && req.checkpoints == 0 {
		log.Info("no changes to submit")
		return nil
	}

	s.sessionMgr.UpdateState(ctx, req.sessionID, session.StateSubmitting, "Submitting changes")

	if err := req.sandbox.ExtractChanges(ctx, req.cloneDir); err != nil {
		return fmt.Errorf("extract changes from VM: %w", err)
	}

	behavior := req.behavior
	title := req.agentResult.Title
	if title == "" {
		title = "Address review feedback"
	}
	commitMsg := title
	if req.agentResult.Description != "" {
		commitMsg = title + "\n\n" + req.agentResult.Description
	}
	tmplData := prTemplateData{
		Title:       title,
		Description: req.agentResult.Description,
		SessionID:   req.sessionID,
		Branch:      pr.headBranch,
		Mode:        req.mode.Name,
		Metadata:    req.metadata,
	}
	sites := s.previewSitesFor(req.proj, req.cfg)
	// The pull request is already open, so every site it addresses resolves.
	withLinks := tmplData.withPreviewLinks(sites.links(pr.headBranch, pr.ref))
	commitMsg = coding.ApplyTrailers(commitMsg,
		renderPRTemplates(behavior.PullRequest.CommitTrailers, withLinks, missingKeyError, log))

	// Pushing the PR's own branch name is a fast-forward of one new commit.
	sha, err := req.forgeImpl.SCM().Commit(ctx, scm.CommitOpts{
		RepoDir:     req.cloneDir,
		Branch:      pr.headBranch,
		Message:     commitMsg,
		AuthorName:  behavior.CommitAuthorName,
		AuthorEmail: behavior.CommitAuthorEmail,
	})
	if err != nil {
		return fmt.Errorf("commit to %s: %w", pr.headBranch, err)
	}

	plan.Push = &pushStep{
		Branch:    pr.headBranch,
		Commit:    sha,
		PRRef:     pr.ref,
		PRURL:     pr.url,
		StartHead: pr.headSHA,
	}

	// Only a commit that changed files can make the pull request's text out of
	// date. A run that only merged its base branch adds nothing the text
	// describes.
	if behavior.PullRequest.UpdateOnFollowUp && req.reviser != nil && len(changedFiles) > 0 {
		plan.Revise = &reviseStep{}
		extras.reviser = req.reviser
		extras.footer = func(title, description string) string {
			data := withLinks
			data.Title = title
			data.Description = description
			return renderPRTemplate(behavior.PullRequest.BodyFooter, data, missingKeyZero, log)
		}
	}

	plan.Comment = &commentStep{
		Kind:  commentFollowUp,
		PRRef: pr.ref,
		Input: &commentInput{
			Request:  req.userPrompt,
			Summary:  req.agentResult.Description,
			Worklog:  toWorklogRecords(req.worklog),
			Cost:     req.cost,
			Sections: commentSectionSpecFrom(behavior.PullRequest, forgeconfig.CommentFollowUpCommit),
			Data:     tmplData,
			Preview:  sites,
		},
	}
	return nil
}

// prepareResultComment plans the comment that puts the agent's written result
// on the pull request. It is the delivery a read-only mode uses: a review of
// somebody else's pull request is worth nothing if it only reaches the session
// event log.
//
// The pull request it comments on is the one the run started from, or the one
// an earlier run of the session opened — which is why the session is read
// rather than assumed.
func (s *Service) prepareResultComment(ctx context.Context, req deliveryRequest, plan *deliveryPlan) error {
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
	title := ""
	if req.agentResult != nil {
		result = req.agentResult.Description
		title = req.agentResult.Title
	}
	// This sink commits nothing, so the branch a header can name is the one the
	// run read: the pull request's head, or the base it started from. The
	// preview it can link to is that same branch's.
	branch := req.baseBranch
	if req.pr != nil {
		branch = req.pr.headBranch
	}
	data := prTemplateData{
		Title:       title,
		Description: result,
		SessionID:   req.sessionID,
		Branch:      branch,
		Mode:        req.mode.Name,
		Metadata:    req.metadata,
	}.withPreviewLinks(s.previewLinksFor(req.proj, req.cfg, branch, prRef))
	sections := sectionsFrom(req.behavior.PullRequest, forgeconfig.CommentPRComment, data, req.log)
	body := formatResultComment(req.userPrompt, result, req.valResult,
		req.worklog, sections, req.cost)
	if body == "" {
		// The comment is this mode's entire output, so there is nothing left to
		// deliver — but an empty comment says even less than none.
		req.log.Info("skipping result comment: the run produced nothing to say")
		return nil
	}
	plan.Comment = &commentStep{
		Kind:     commentResult,
		PRRef:    prRef,
		Required: true,
		Body:     body,
	}
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
//
// The result is written with no heading above it. It is the whole point of this
// comment, and the headings a repository asks for under `comment.sections` are
// the structure a reader should meet first — a wrapper heading of kvarn's own
// would only push them a level down.
func formatResultComment(prompt, result string, val *sandbox.ValidationResult,
	entries []worklogEntry, sections commentSections, report cost.Report,
) string {
	var sb strings.Builder
	writeHeader(&sb, sections.header)
	writeVerbatim(&sb, result)
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
