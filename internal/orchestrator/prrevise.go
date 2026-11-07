package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/session"
)

// prRevision is what revising a pull request's title and description after a
// follow-up commit needs.
type prRevision struct {
	sessionID string
	project   string
	pr        *prTarget
	forgeImpl forge.Forge
	cloneURL  string
	creds     scm.CredentialSource
	reviser   agent.PullRequestReviser
	// footer renders the operator's footer for a revised title and
	// description, so the footer a revised body carries is rendered against the
	// text it sits under, the same way a new pull request's is.
	footer func(title, description string) string
	log    *slog.Logger
}

// revisePullRequestText brings the title and description of a pull request
// kvarn opened in step with the follow-up commit just pushed onto it. It
// returns a line for the follow-up comment that says what it did, or "" when
// there is nothing worth saying.
//
// A squash merge takes its message from the title and description. Without
// this they keep describing the first attempt, and every change review asked
// for is missing from the history the merge writes.
//
// Two rules keep it from overwriting anything a person wrote:
//
//   - The pull request must be kvarn's: some session on record wrote its text.
//     A pull request a person opened has no such session.
//   - Each field is rewritten only while the forge still holds exactly what
//     kvarn last wrote there. A person who edits the title keeps their title,
//     and the body is still revised if it is untouched.
//
// Nothing here fails the run. The commit has already landed, and text that is
// out of date is a smaller problem than a run reported as failed after its work
// was delivered.
func (s *Service) revisePullRequestText(ctx context.Context, in prRevision) string {
	log := in.log
	written, err := s.lastWrittenPRText(ctx, in.project, in.pr.ref)
	if err != nil {
		log.Warn("could not look up the pull request text kvarn wrote; leaving it alone", "error", err)
		return ""
	}
	if written == nil {
		log.Info("leaving the pull request text alone: no session on record wrote it")
		return ""
	}

	// The record moves to this session whatever happens below, so the newest
	// session on the pull request always holds it. Retention prunes old
	// sessions first, and a pull request that stays active must not lose its
	// record because the session that opened it was pruned.
	record := *written
	defer func() {
		if err := s.sessionMgr.SetPullRequestWritten(context.WithoutCancel(ctx), in.sessionID, record); err != nil {
			log.Warn("failed to record the pull request text", "error", err)
		}
	}()

	getOpts := forge.GetPROpts{RepoURL: in.cloneURL, PRRef: in.pr.ref, Credentials: in.creds}
	current, err := in.forgeImpl.GetPullRequest(ctx, getOpts)
	if err != nil {
		log.Warn("could not read the pull request; leaving its text alone", "error", err)
		return ""
	}
	titleOurs := samePRText(current.Title, written.Title)
	bodyOurs := samePRText(current.Body, written.Body)
	if !titleOurs && !bodyOurs {
		log.Info("leaving the pull request text alone: both fields were edited by hand")
		return prRevisionNote(false, false, true, true)
	}

	// The revision starts from the description without its footer when the
	// body is still kvarn's. A body a person edited is shown as it is, as
	// context for the title only.
	description := written.Description
	if !bodyOurs {
		description = current.Body
	}
	diff, err := in.forgeImpl.GetPullRequestDiff(ctx, getOpts)
	if err != nil {
		log.Warn("could not read the pull request diff; revising without it", "error", err)
		diff = ""
	}
	revised, err := in.reviser.RevisePullRequest(ctx, agent.PullRequestRevision{
		Title:       current.Title,
		Description: description,
		Diff:        diff,
	})
	if err != nil {
		log.Warn("could not revise the pull request text; leaving it alone", "error", err)
		return ""
	}

	// Read again before writing. The revision was a model call, and a person
	// may have edited the text while it ran.
	latest, err := in.forgeImpl.GetPullRequest(ctx, getOpts)
	if err != nil {
		log.Warn("could not read the pull request again; leaving its text alone", "error", err)
		return ""
	}
	titleOurs = titleOurs && samePRText(latest.Title, written.Title)
	bodyOurs = bodyOurs && samePRText(latest.Body, written.Body)

	update := forge.UpdatePROpts{RepoURL: in.cloneURL, PRRef: in.pr.ref, Credentials: in.creds}
	title := strings.TrimSpace(revised.Title)
	// The footer renders against the title the pull request will carry, which
	// is the person's own when the title is not kvarn's to change.
	finalTitle := latest.Title
	if titleOurs && title != "" && !samePRText(title, latest.Title) {
		update.Title = &title
		finalTitle = title
	}
	// The description is compared without the footer. The footer names the
	// run that wrote it, so it differs on every run, and a body rewritten only
	// to change it would be noise in the pull request's history.
	body := coding.ApplyFooter(revised.Description, in.footer(finalTitle, revised.Description))
	if bodyOurs && strings.TrimSpace(revised.Description) != "" &&
		!samePRText(revised.Description, written.Description) {
		update.Body = &body
	}

	if update.Title == nil && update.Body == nil {
		log.Info("pull request text is still accurate; leaving it as it is")
		return prRevisionNote(false, false, !titleOurs, !bodyOurs)
	}
	if err := in.forgeImpl.UpdatePullRequest(ctx, update); err != nil {
		log.Warn("failed to update the pull request text", "error", err)
		return ""
	}
	if update.Title != nil {
		record.Title = title
	}
	if update.Body != nil {
		record.Body = body
		record.Description = revised.Description
	}
	log.Info("updated the pull request text",
		"title", update.Title != nil, "description", update.Body != nil)
	return prRevisionNote(update.Title != nil, update.Body != nil, !titleOurs, !bodyOurs)
}

// lastWrittenPRText returns the pull request text the newest session on record
// wrote, or nil when no session wrote any.
func (s *Service) lastWrittenPRText(ctx context.Context, project, prRef string) (*session.PullRequestText, error) {
	sessions, err := s.sessionMgr.List(ctx, session.SessionFilter{Project: project, PRRef: prRef})
	if err != nil {
		return nil, err
	}
	// The listing is newest first.
	for _, sess := range sessions {
		if sess.PullRequestWritten != nil {
			return sess.PullRequestWritten, nil
		}
	}
	return nil, nil
}

// samePRText reports whether two pull request texts are the same once line
// endings and surrounding whitespace are ignored. A forge's web editor can
// save line endings differently from how the API wrote them, and that alone is
// not a person changing the text.
func samePRText(a, b string) bool {
	return normalizePRText(a) == normalizePRText(b)
}

func normalizePRText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// prRevisionNote is the line the follow-up comment carries about the pull
// request text: what was updated, and what was left alone because a person
// edited it. Silence about a field means it was still accurate.
func prRevisionNote(titleUpdated, bodyUpdated, titleEdited, bodyEdited bool) string {
	var parts []string
	if updated := prFieldNames(titleUpdated, bodyUpdated); updated != "" {
		parts = append(parts, fmt.Sprintf("Updated the pull request %s to match the change as it is now.", updated))
	}
	if edited := prFieldNames(titleEdited, bodyEdited); edited != "" {
		verb, pronoun := "was", "it as it is"
		if titleEdited && bodyEdited {
			verb, pronoun = "were", "them as they are"
		}
		parts = append(parts, fmt.Sprintf("The %s %s edited by hand, so this run left %s.", edited, verb, pronoun))
	}
	return strings.Join(parts, " ")
}

func prFieldNames(title, body bool) string {
	switch {
	case title && body:
		return "title and description"
	case title:
		return "title"
	case body:
		return "description"
	default:
		return ""
	}
}
