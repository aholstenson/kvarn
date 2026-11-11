package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/scm"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/session"
)

// deliveryPlan is a run's output on its way to the forge, written down so the
// delivery can be repeated: after a forge or network failure, and after a
// restart of the orchestrator.
//
// It holds everything the publish steps need and nothing that only the run
// had: the commit is already made in the clone the plan travels with, and
// every text is either rendered or kept as the inputs it is rendered from.
// Credentials are not part of it. Each attempt resolves them again from the
// project, because a delivery can outlive any token it started with.
//
// Progress records which steps have landed. A step that has landed is never
// done again, and a step that may have landed without the answer reaching
// kvarn looks on the forge first. Together these are what keep a retry from
// opening a second pull request or posting a second comment.
type deliveryPlan struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	// FailAfter is the error the session fails with once the delivery has
	// landed, for a run whose verdict was negative but still had to be
	// delivered. Empty when a delivered run is a completed one.
	FailAfter string `json:"fail_after,omitempty"`

	Push        *pushStep        `json:"push,omitempty"`
	PullRequest *pullRequestStep `json:"pull_request,omitempty"`
	Revise      *reviseStep      `json:"revise,omitempty"`
	Comment     *commentStep     `json:"comment,omitempty"`

	Progress deliveryProgress `json:"progress"`
}

// pushStep sends the prepared commit to the forge.
type pushStep struct {
	// Branch is the branch in the clone and on the forge.
	Branch string `json:"branch"`
	// Commit is the commit Branch points at in the clone.
	Commit string `json:"commit"`
	// PRRef and StartHead are set for a follow-up commit onto an existing
	// pull request: the pull request, and the head it had when the run
	// started. The push only goes ahead while the head is still there, since
	// pushing over somebody else's newer commit would revert it.
	PRRef     string `json:"pr_ref,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	StartHead string `json:"start_head,omitempty"`
}

// pullRequestStep opens the pull request the pushed branch belongs to.
type pullRequestStep struct {
	BaseBranch string `json:"base_branch"`
	Title      string `json:"title"`
	// Body is the body as sent, footer included, and Description the same
	// text without the footer. Both are recorded on the session as the text
	// kvarn wrote.
	Body        string   `json:"body"`
	Description string   `json:"description"`
	Labels      []string `json:"labels,omitempty"`
}

// reviseStep brings the title and description of the pull request a
// follow-up commit landed on in step with that commit. It needs the run's
// agent, which only the first attempt has, so a delivery that resumes later
// leaves the text as it is.
type reviseStep struct{}

// commentKind says which comment a commentStep posts.
type commentKind string

const (
	commentNewPullRequest commentKind = "new_pull_request"
	commentFollowUp       commentKind = "follow_up"
	commentResult         commentKind = "result"
)

// commentStep posts the run's comment on the pull request.
type commentStep struct {
	Kind commentKind `json:"kind"`
	// PRRef is the pull request to comment on. Empty means the one the
	// delivery opens.
	PRRef string `json:"pr_ref,omitempty"`
	// Required makes a comment that the forge refuses fail the delivery. It
	// is set when the comment is the run's whole output. A comment that only
	// describes a commit that has already landed is not worth failing a
	// delivered run over.
	Required bool `json:"required,omitempty"`
	// Body is the comment, already rendered, for a comment that depends on
	// nothing the delivery learns. Input is set instead when the comment names
	// the pull request the delivery opens, or what the revision did.
	Body  string        `json:"body,omitempty"`
	Input *commentInput `json:"input,omitempty"`
}

// commentInput is what a comment is rendered from at the moment it is posted.
type commentInput struct {
	// Request is the task or the feedback the run was given.
	Request string `json:"request"`
	// Summary is the agent's account of a follow-up commit.
	Summary  string             `json:"summary,omitempty"`
	Worklog  []worklogRecord    `json:"worklog,omitempty"`
	Cost     cost.Report        `json:"cost"`
	Sections commentSectionSpec `json:"sections"`
	// Data is what the operator's header is rendered against, without the
	// preview addresses, which are resolved from Preview when the comment is
	// rendered.
	Data    prTemplateData `json:"data"`
	Preview *previewSites  `json:"preview,omitempty"`
}

// commentSectionSpec is commentSections before the header is rendered.
type commentSectionSpec struct {
	Header  string                `json:"header,omitempty"`
	Worklog bool                  `json:"worklog,omitempty"`
	Cost    bool                  `json:"cost,omitempty"`
	Quote   forgeconfig.QuoteMode `json:"quote,omitempty"`
}

// worklogRecord is a worklogEntry in a form that can be stored.
type worklogRecord struct {
	Kind    worklogKind `json:"kind"`
	ToolID  string      `json:"tool_id,omitempty"`
	Args    string      `json:"args,omitempty"`
	Text    string      `json:"text,omitempty"`
	IsError bool        `json:"is_error,omitempty"`
}

func toWorklogRecords(entries []worklogEntry) []worklogRecord {
	out := make([]worklogRecord, len(entries))
	for i, e := range entries {
		out[i] = worklogRecord{Kind: e.kind, ToolID: e.toolID, Args: e.args, Text: e.text, IsError: e.isError}
	}
	return out
}

func fromWorklogRecords(records []worklogRecord) []worklogEntry {
	out := make([]worklogEntry, len(records))
	for i, r := range records {
		out[i] = worklogEntry{kind: r.Kind, toolID: r.ToolID, args: r.Args, text: r.Text, isError: r.IsError}
	}
	return out
}

// deliveryProgress is which steps of a plan have landed.
type deliveryProgress struct {
	Pushed bool `json:"pushed,omitempty"`

	// PRTried records that a create has been sent at least once, so every
	// later attempt looks for the pull request before it creates one.
	PRTried bool   `json:"pr_tried,omitempty"`
	PRRef   string `json:"pr_ref,omitempty"`
	PRURL   string `json:"pr_url,omitempty"`

	Labeled bool `json:"labeled,omitempty"`

	// Revised records that the revision step is over, and Note is the line
	// it left for the comment.
	Revised bool   `json:"revised,omitempty"`
	Note    string `json:"note,omitempty"`

	// CommentTried is when the comment was first sent, so a later attempt
	// knows to look for it and from when.
	CommentTried time.Time `json:"comment_tried,omitzero"`
	Commented    bool      `json:"commented,omitempty"`
}

// deliveryEnv is what one attempt resolves before it starts: the forge and the
// credentials, resolved again each time so a delivery is not held to a token
// that expired while it waited.
type deliveryEnv struct {
	proj     *project.Project
	forge    forge.Forge
	creds    scm.CredentialSource
	cloneURL string
	// repoDir is the clone holding the prepared commit.
	repoDir string
}

// deliveryExtras is what only the attempt made inside the run has. A delivery
// that resumes later goes without it.
type deliveryExtras struct {
	reviser agent.PullRequestReviser
	// footer renders the operator's footer for a revised title and
	// description.
	footer func(title, description string) string
	// settle runs once the first attempt is over, before the session can
	// reach a final state or the delivery is handed off. The run uses it for
	// its final cost record, which has to include what the revision spent.
	settle func()
	// jobCtx is the run's own context. A cancel that reaches the run before
	// the worker owns the delivery lands there, so the worker carries it over.
	jobCtx context.Context
}

// stepError is a failed step, with the answer to whether the same step can
// succeed later.
type stepError struct {
	err        error
	transient  bool
	retryAfter time.Duration
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// forgeStepError classifies a failed forge call.
func forgeStepError(err error, format string, args ...any) error {
	return &stepError{
		err:        fmt.Errorf(format+": %w", append(args, err)...),
		transient:  forge.IsTransient(err),
		retryAfter: forge.RetryAfterOf(err),
	}
}

// permanentStepError is a failure that no later attempt repairs.
func permanentStepError(err error) error {
	return &stepError{err: err}
}

// isTransientDelivery reports whether a failed attempt is worth repeating. A
// failure that no step classified — the session store refusing a write, most
// often — is taken as transient: every step can be repeated safely, and giving
// up would throw away work a later attempt can still deliver.
func isTransientDelivery(err error) (bool, time.Duration) {
	var se *stepError
	if errors.As(err, &se) {
		return se.transient, se.retryAfter
	}
	return true, 0
}

// deliveryMarker is the hidden line a delivery's comment ends with. It is
// what a retry looks for on the forge to learn whether a post whose answer
// was lost has landed.
func deliveryMarker(sessionID string) string {
	return fmt.Sprintf("<!-- kvarn:delivery %s -->", sessionID)
}

// publish runs the steps of plan that have not landed yet, in order. save
// persists the plan's progress and is called after every step that lands, so
// a failure or a restart resumes after the last step that is known to be done.
func (s *Service) publish(
	ctx context.Context, plan *deliveryPlan, env *deliveryEnv, extras *deliveryExtras,
	save func() error, log *slog.Logger,
) error {
	p := &plan.Progress

	if plan.Push != nil && !p.Pushed {
		if err := s.publishPush(ctx, plan.Push, env, log); err != nil {
			return err
		}
		p.Pushed = true
		if err := save(); err != nil {
			return err
		}
	}

	if plan.PullRequest != nil && p.PRRef == "" {
		if err := s.publishPullRequest(ctx, plan, env, save, log); err != nil {
			return err
		}
	}

	if plan.PullRequest != nil && len(plan.PullRequest.Labels) > 0 && !p.Labeled {
		err := env.forge.AddLabels(ctx, forge.AddLabelsOpts{
			RepoURL:     env.cloneURL,
			PRRef:       p.PRRef,
			Labels:      plan.PullRequest.Labels,
			Credentials: env.creds,
		})
		if err != nil {
			if forge.IsTransient(err) {
				return forgeStepError(err, "add labels to pull request %s", p.PRRef)
			}
			log.Warn("failed to add labels to the pull request", "error", err)
		}
		p.Labeled = true
		if err := save(); err != nil {
			return err
		}
	}

	if plan.Revise != nil && !p.Revised {
		if extras != nil && extras.reviser != nil {
			p.Note = s.revisePullRequestText(ctx, prRevision{
				sessionID: plan.SessionID,
				project:   plan.Project,
				pr:        &prTarget{ref: plan.Push.PRRef, headBranch: plan.Push.Branch, url: plan.Push.PRURL},
				forgeImpl: env.forge,
				cloneURL:  env.cloneURL,
				creds:     env.creds,
				reviser:   extras.reviser,
				footer:    extras.footer,
				log:       log,
			})
		} else {
			log.Info("leaving the pull request text alone: the delivery resumed after the run that could revise it")
		}
		p.Revised = true
		if err := save(); err != nil {
			return err
		}
	}

	if plan.Comment != nil && !p.Commented {
		if err := s.publishComment(ctx, plan, env, save, log); err != nil {
			return err
		}
		p.Commented = true
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// publishPush sends the prepared commit.
func (s *Service) publishPush(ctx context.Context, step *pushStep, env *deliveryEnv, log *slog.Logger) error {
	if step.PRRef != "" {
		// A follow-up commit lands on a branch other people can push to, so
		// the head is checked before every push rather than once.
		current, err := env.forge.GetPullRequest(ctx, forge.GetPROpts{
			RepoURL:     env.cloneURL,
			PRRef:       step.PRRef,
			Credentials: env.creds,
		})
		if err != nil {
			return forgeStepError(err, "re-read pull request %s", step.PRRef)
		}
		switch current.HeadSHA {
		case step.Commit:
			// An earlier attempt pushed and never learned that it had.
			log.Info("the follow-up commit is already on the pull request", "commit", step.Commit)
			s.recordMirrorPush(ctx, env.proj, env.cloneURL, env.repoDir, step.Branch, log)
			return nil
		case step.StartHead:
		default:
			return permanentStepError(fmt.Errorf("pull request %s moved during the run (head %s -> %s); not pushing",
				step.PRRef, step.StartHead, current.HeadSHA))
		}
	}

	if err := env.forge.SCM().Push(ctx, scm.PushOpts{
		RepoDir:     env.repoDir,
		RemoteURL:   env.cloneURL,
		Branch:      step.Branch,
		Credentials: env.creds,
	}); err != nil {
		return &stepError{
			err:       fmt.Errorf("push to %s: %w", step.Branch, err),
			transient: gitscm.IsTransientPushError(err),
		}
	}
	s.recordMirrorPush(ctx, env.proj, env.cloneURL, env.repoDir, step.Branch, log)
	log.Info("pushed delivery branch", "branch", step.Branch, "commit", step.Commit)
	return nil
}

// publishPullRequest opens the pull request, or finds the one an earlier
// attempt opened, and records it on the session.
func (s *Service) publishPullRequest(
	ctx context.Context, plan *deliveryPlan, env *deliveryEnv, save func() error, log *slog.Logger,
) error {
	step, p := plan.PullRequest, &plan.Progress
	branch := plan.Push.Branch

	var pr *forge.PullRequest
	if p.PRTried {
		found, err := env.forge.FindPullRequest(ctx, forge.FindPROpts{
			RepoURL:     env.cloneURL,
			HeadBranch:  branch,
			Credentials: env.creds,
		})
		if err != nil {
			return forgeStepError(err, "look for the pull request of %s", branch)
		}
		pr = found
	}

	if pr == nil {
		p.PRTried = true
		if err := save(); err != nil {
			return err
		}
		created, err := env.forge.CreatePullRequest(ctx, forge.CreatePROpts{
			RepoURL:     env.cloneURL,
			BaseBranch:  step.BaseBranch,
			HeadBranch:  branch,
			Title:       step.Title,
			Body:        step.Body,
			Credentials: env.creds,
		})
		if err != nil {
			// A pull request for the branch may exist already, opened by an
			// attempt whose answer was lost. GitHub answers that with 422,
			// which is otherwise permanent.
			if found, findErr := env.forge.FindPullRequest(ctx, forge.FindPROpts{
				RepoURL:     env.cloneURL,
				HeadBranch:  branch,
				Credentials: env.creds,
			}); findErr == nil && found != nil {
				created, err = found, nil
			}
		}
		if err != nil {
			return forgeStepError(err, "create pull request for %s", branch)
		}
		pr = created
	} else {
		log.Info("found the pull request an earlier attempt opened", "url", pr.URL)
	}

	log.Info("pull request created", "url", pr.URL, "ref", pr.Ref)
	// The ref is what a later feedback run is looked up by, and the pull
	// request exists on the forge now, so the write is not cancelled with the
	// attempt.
	wctx := context.WithoutCancel(ctx)
	if err := s.sessionMgr.SetPullRequest(wctx, plan.SessionID, pr.URL, pr.Ref, branch); err != nil {
		return fmt.Errorf("record pull request %s: %w", pr.URL, err)
	}
	// The text kvarn wrote is what makes the pull request its own to revise on
	// a later follow-up. Losing it only makes that follow-up leave the text
	// alone, so a failure is a warning.
	if err := s.sessionMgr.SetPullRequestWritten(wctx, plan.SessionID, session.PullRequestText{
		Title:       step.Title,
		Body:        step.Body,
		Description: step.Description,
	}); err != nil {
		log.Warn("failed to record the pull request text", "error", err)
	}
	p.PRRef, p.PRURL = pr.Ref, pr.URL
	return save()
}

// publishComment posts the comment unless an earlier attempt already did.
func (s *Service) publishComment(
	ctx context.Context, plan *deliveryPlan, env *deliveryEnv, save func() error, log *slog.Logger,
) error {
	step, p := plan.Comment, &plan.Progress
	prRef := step.PRRef
	if prRef == "" {
		prRef = p.PRRef
	}
	if prRef == "" {
		return permanentStepError(errors.New("no pull request to comment on"))
	}

	body := s.renderDeliveryComment(step, prRef, p.Note, log)
	if body == "" {
		log.Info("skipping the delivery comment: there is nothing to say")
		return nil
	}
	marker := deliveryMarker(plan.SessionID)

	if !p.CommentTried.IsZero() {
		// The clocks of this host and the forge need not agree, so the search
		// starts a margin before the first post.
		found, err := env.forge.HasComment(ctx, forge.HasCommentOpts{
			RepoURL:     env.cloneURL,
			PRRef:       prRef,
			Marker:      marker,
			Since:       p.CommentTried.Add(-10 * time.Minute),
			Credentials: env.creds,
		})
		if err != nil {
			return forgeStepError(err, "look for the comment on %s", prRef)
		}
		if found {
			log.Info("found the comment an earlier attempt posted", "pr_ref", prRef)
			return nil
		}
	} else {
		p.CommentTried = time.Now().UTC()
		if err := save(); err != nil {
			return err
		}
	}

	err := env.forge.PostComment(ctx, forge.PostCommentOpts{
		RepoURL:     env.cloneURL,
		PRRef:       prRef,
		Body:        withDeliveryMarker(marker, body),
		Credentials: env.creds,
	})
	if err == nil {
		log.Info("delivery comment posted", "pr_ref", prRef)
		return nil
	}
	if !step.Required && !forge.IsTransient(err) {
		// The change itself has landed; a comment the forge refuses is not
		// worth failing the run over.
		log.Warn("failed to post the delivery comment", "error", err)
		return nil
	}
	return forgeStepError(err, "post %s comment on %s", step.Kind.label(), prRef)
}

// label names the kind of comment in an error message.
func (k commentKind) label() string {
	switch k {
	case commentResult:
		return "result"
	case commentFollowUp:
		return "follow-up"
	default:
		return "work log"
	}
}

// withDeliveryMarker puts the marker after the body, trimming the body so the
// two together still fit what the forge accepts. It goes last because the
// operator's header is written to be the first thing a reader of the raw text
// meets, and the trim is what keeps the marker from being cut off.
func withDeliveryMarker(marker, body string) string {
	room := maxCommentBody - len(marker) - 1
	if len(body) > room {
		cut := room - len(truncationNote)
		for cut > 0 && !utf8RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut] + truncationNote
	}
	return body + "\n" + marker
}

// renderDeliveryComment renders the comment for the pull request prRef, with
// note as the line about the revision of its text.
func (s *Service) renderDeliveryComment(step *commentStep, prRef, note string, log *slog.Logger) string {
	if step.Input == nil {
		return step.Body
	}
	in := step.Input
	data := in.Data.withPreviewLinks(in.Preview.links(in.Data.Branch, prRef))
	sections := commentSections{
		header:  renderPRTemplate(in.Sections.Header, data, missingKeyZero, log),
		worklog: in.Sections.Worklog,
		cost:    in.Sections.Cost,
		quote:   in.Sections.Quote,
	}
	worklog := fromWorklogRecords(in.Worklog)
	switch step.Kind {
	case commentFollowUp:
		return formatFollowupComment(in.Request, in.Summary, note, worklog, sections, in.Cost)
	default:
		return formatWorklogComment(in.Request, worklog, sections, in.Cost)
	}
}

// commentSectionSpecFrom reads the comment choices for kind out of resolved
// pull-request content, with the header left as its template.
func commentSectionSpecFrom(c forgeconfig.Content, kind forgeconfig.CommentKind) commentSectionSpec {
	return commentSectionSpec{
		Header:  c.CommentHeaders.For(kind),
		Worklog: c.ReportWorklog,
		Cost:    c.ReportCost,
		Quote:   c.QuoteTask,
	}
}

// describeDeliveryError is the short form of a failed attempt for the session
// message.
func describeDeliveryError(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	const max = 200
	if len(msg) > max {
		cut := max
		for cut > 0 && !utf8RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "…"
	}
	return msg
}
