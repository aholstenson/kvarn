package orchestrator

import (
	"bytes"
	"context"
	"log/slog"
	"text/template"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	"github.com/aholstenson/kvarn/internal/config/project"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

// resolvePRBehavior settles what this job's pull request, commit and comments
// will say, layering the global defaults, the forge, the project and the
// repository's own `pull_request:` block.
//
// It runs once per job, before the agent starts, because both ends need the
// answer: the agent's system prompt carries the comment conventions, and the
// delivery carries everything else.
func (s *Service) resolvePRBehavior(
	ctx context.Context,
	proj *project.Project,
	forgeCfg *forgeconfig.ForgeConfig,
	cfg *projconfig.Config,
	userDefaults modelcfg.Defaults,
	modeName string,
	log *slog.Logger,
) forgeconfig.Behavior {
	var forgeDefaults forgeconfig.Defaults
	if s.forgeDefaults != nil {
		d, err := s.forgeDefaults.Defaults(ctx)
		if err != nil {
			log.Warn("failed to load forge defaults; using built-ins", "error", err)
		} else {
			forgeDefaults = d
		}
	}
	forgeDefaults.PullRequest = withLegacyReporting(forgeDefaults.PullRequest,
		userDefaults.ReportWorklogOnPR, userDefaults.ReportCostOnPR,
		log, "agents.toml [defaults]")

	overrides := forgeconfig.Overrides{
		BranchPrefix:      proj.BranchPrefix,
		CommitAuthorName:  proj.CommitAuthorName,
		CommitAuthorEmail: proj.CommitAuthorEmail,
		Labels:            proj.Labels,
		PullRequest: withLegacyReporting(proj.PullRequest,
			proj.ReportWorklogOnPR, proj.ReportCostOnPR,
			log, "projects.toml [projects."+proj.Name+"]"),
	}

	var repo forgeconfig.RepoContent
	if cfg != nil {
		repo = cfg.PullRequest.Resolve(modeName)
	}

	return forgeCfg.ResolveBehavior(forgeDefaults, overrides, repo)
}

// withLegacyReporting fills the report toggles from their superseded top-level
// spellings when the `pull_request` block does not set them.
//
// The settings moved so they sit with the content they gate rather than in the
// cost resolver. Reading the old spelling keeps existing configs working; the
// warning is what tells an operator to move it, and it only fires when the old
// key actually decided the outcome.
func withLegacyReporting(
	c forgeconfig.PRContent, legacyWorklog, legacyCost *bool, log *slog.Logger, where string,
) forgeconfig.PRContent {
	if c.ReportWorklog == nil && legacyWorklog != nil {
		log.Warn("report_worklog_on_pr is deprecated at the top level; move it into the pull_request block",
			"location", where)
		c.ReportWorklog = legacyWorklog
	}
	if c.ReportCost == nil && legacyCost != nil {
		log.Warn("report_cost_on_pr is deprecated at the top level; move it into the pull_request block",
			"location", where)
		c.ReportCost = legacyCost
	}
	return c
}

// sectionsFrom reads the comment-section choices out of resolved pull-request
// content, expanding the header configured for this kind of comment. Each kind
// carries its own header, so the caller has to say which one it is building.
//
// The header renders leniently: it is the one piece of comment config written
// against keys only some submissions carry, so a template that guards a line it
// cannot fill has to survive the guard.
func sectionsFrom(
	c forgeconfig.Content, kind forgeconfig.CommentKind, data prTemplateData, log *slog.Logger,
) commentSections {
	return commentSections{
		header:  renderPRTemplate(c.CommentHeaders.For(kind), data, missingKeyZero, log),
		worklog: c.ReportWorklog,
		cost:    c.ReportCost,
		quote:   c.QuoteTask,
	}
}

// prTemplateData is what a comment header, body footer or commit trailer is
// rendered against. The fixed fields are deliberately few: they identify a run,
// and widening what they can reach turns operator config into a way to pull
// arbitrary run content into a commit message.
//
// Metadata is the exception, and a bounded one. It is the submission's own
// annotations, capped at submission and already readable by anyone who can read
// the session, so an operator naming a key here publishes an identifier their
// own system chose to attach — which is the point: it is what connects a pull
// request back to the ticket that asked for it.
//
// The preview addresses are bounded the same way. A hostname is expanded from
// the repository's own site patterns, but every expansion is checked to sit
// inside the domain the operator configured, so a branch cannot use a comment
// header to publish a link to somewhere else.
type prTemplateData struct {
	Title       string
	Description string
	SessionID   string
	Branch      string
	Mode        string
	Metadata    map[string]string
	// PreviewURL is the address of this branch's preview, and PreviewURLs is
	// every declared site by name. Both are empty whenever no address can be
	// formed — previews off, no `preview:` block, or a `{pr}` pattern on a run
	// that has no pull request — which is what a template guards on.
	PreviewURL  string
	PreviewURLs map[string]string
}

// withPreviewLinks returns the data with the preview addresses filled in. A
// comment is built after the run has opened its pull request, so the links it
// carries are resolved later than the rest of the data.
func (d prTemplateData) withPreviewLinks(links previewLinks) prTemplateData {
	d.PreviewURL = links.Primary
	d.PreviewURLs = links.Sites
	return d
}

// Go templates only reach a map key through the `.Metadata.key` field syntax
// when the key is a bare identifier, and metadata keys may also hold "-", "."
// and "/". `index` is what covers the rest, so both spellings are documented.

// missingKeyMode says what a template does when it reads a metadata key the
// submission did not carry. It only ever affects map lookups: a misspelled
// fixed field fails under either mode.
type missingKeyMode string

const (
	// missingKeyZero renders an absent key as the empty string. Guarding a line
	// with `{{ if .Metadata.issue_id }}` needs this: the guard has to evaluate
	// the key before it can test it, so under the strict mode the very template
	// written to tolerate a missing key is the one that fails on it.
	missingKeyZero missingKeyMode = "missingkey=zero"
	// missingKeyError drops the whole template when a key is absent. It is for
	// text that lands somewhere unfixable, where "Issue-Id: " with nothing after
	// it is worse than no line at all.
	missingKeyError missingKeyMode = "missingkey=error"
)

// renderPRTemplate expands one header, footer or trailer. A template that does
// not parse or execute is dropped with a warning rather than published: a pull
// request carrying a literal "{{ .SessionID }}" reads as a kvarn bug, while a
// missing footer reads as the configuration error it is.
func renderPRTemplate(tmpl string, data prTemplateData, missing missingKeyMode, log *slog.Logger) string {
	if tmpl == "" {
		return ""
	}
	t, err := template.New("pr").Option(string(missing)).Parse(tmpl)
	if err != nil {
		log.Warn("pull request template does not parse; leaving it out", "template", tmpl, "error", err)
		return ""
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		log.Warn("pull request template failed to render; leaving it out", "template", tmpl, "error", err)
		return ""
	}
	return buf.String()
}

// renderPRTemplates expands a list, dropping the entries that fail.
func renderPRTemplates(tmpls []string, data prTemplateData, missing missingKeyMode, log *slog.Logger) []string {
	var out []string
	for _, t := range tmpls {
		if rendered := renderPRTemplate(t, data, missing, log); rendered != "" {
			out = append(out, rendered)
		}
	}
	return out
}
