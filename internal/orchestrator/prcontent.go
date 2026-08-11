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
// content.
func sectionsFrom(c forgeconfig.Content) commentSections {
	return commentSections{worklog: c.ReportWorklog, cost: c.ReportCost, quote: c.QuoteTask}
}

// prTemplateData is what a body footer or commit trailer is rendered against.
// The set is deliberately small: these strings identify a run, and widening
// what they can reach turns operator config into a way to pull arbitrary run
// content into a commit message.
type prTemplateData struct {
	Title       string
	Description string
	SessionID   string
	Branch      string
	Mode        string
}

// renderPRTemplate expands one footer or trailer. A template that does not
// parse or execute is dropped with a warning rather than published: a pull
// request carrying a literal "{{ .SessionID }}" reads as a kvarn bug, while a
// missing footer reads as the configuration error it is.
func renderPRTemplate(tmpl string, data prTemplateData, log *slog.Logger) string {
	if tmpl == "" {
		return ""
	}
	t, err := template.New("pr").Option("missingkey=error").Parse(tmpl)
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
func renderPRTemplates(tmpls []string, data prTemplateData, log *slog.Logger) []string {
	var out []string
	for _, t := range tmpls {
		if rendered := renderPRTemplate(t, data, log); rendered != "" {
			out = append(out, rendered)
		}
	}
	return out
}
