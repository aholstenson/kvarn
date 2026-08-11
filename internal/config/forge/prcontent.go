package forge

import (
	"fmt"
	"strings"
)

// Compiled-in fallbacks for pull-request content, used when no layer sets a
// value.
const (
	// DefaultTitleMaxLength is the budget a generated title is asked to fit.
	// It matches the conventional git subject width.
	DefaultTitleMaxLength = 72
	// DefaultReportWorklog and DefaultReportCost keep a fresh install showing
	// what a run did and what it cost.
	DefaultReportWorklog = true
	DefaultReportCost    = true
	// DefaultQuoteTask keeps short requests visible and folds long ones away,
	// which is the behaviour that needs no thought from an operator.
	DefaultQuoteTask = QuoteAuto
)

// QuoteMode says how much of the request that started a run — the task, or the
// feedback a follow-up addressed — the comment a delivery posts quotes back.
//
// The quote is provenance: valuable for answering "what asked for this?", but
// it is arbitrary-length user text, and rendered in full it pushes the result
// the reader came for off the screen.
type QuoteMode string

const (
	// QuoteInherit is the unset value, which takes the next layer down.
	QuoteInherit QuoteMode = ""
	// QuoteAuto renders a short request inline and folds a long one into a
	// collapsed block.
	QuoteAuto QuoteMode = "auto"
	// QuoteCollapsed always folds the request away, however short it is.
	QuoteCollapsed QuoteMode = "collapsed"
	// QuoteFull always renders the request in full.
	QuoteFull QuoteMode = "full"
	// QuoteOff leaves the request out. The session event log still has it.
	QuoteOff QuoteMode = "off"
)

// quoteModes is every value a config file may spell.
var quoteModes = []QuoteMode{QuoteAuto, QuoteCollapsed, QuoteFull, QuoteOff}

// UnmarshalText parses a quote mode, rejecting anything unrecognised. Failing
// the load is what tells an operator about a typo; silently treating "colapsed"
// as unset would leave them looking at comments that ignore their config.
func (m *QuoteMode) UnmarshalText(text []byte) error {
	s := QuoteMode(strings.TrimSpace(string(text)))
	if s == QuoteInherit {
		*m = QuoteInherit
		return nil
	}
	for _, known := range quoteModes {
		if s == known {
			*m = known
			return nil
		}
	}
	names := make([]string, len(quoteModes))
	for i, known := range quoteModes {
		names[i] = string(known)
	}
	return fmt.Errorf("unknown quote mode %q (want one of: %s)", s, strings.Join(names, ", "))
}

// Section is one heading a generated pull-request body or comment carries. The
// agent is asked to fill it in, and the delivery renders the sections in the
// order they are declared regardless of the order they come back in.
type Section struct {
	// Name is the heading text, rendered as a level-2 markdown heading.
	Name string
	// Description tells the agent what belongs under the heading.
	Description string
	// Required makes a missing or empty section worth a second ask. A section
	// still missing after that is rendered as explicitly not provided rather
	// than dropped, so a reader sees the gap instead of a body that looks
	// complete.
	Required bool
}

// PRContent is the pull-request content one operator-owned layer contributes:
// the `[defaults.pull_request]` and `[forges.<name>.pull_request]` blocks in
// forges.toml, and `[projects.<name>.pull_request]` in projects.toml.
//
// It carries no sections. Section structure is authoring work that belongs with
// the repository that knows what its pull requests should say, and TOML inline
// tables cannot hold a multi-line description without one very long line.
type PRContent struct {
	TitleInstructions   string
	TitleMaxLength      *int
	BodyInstructions    string
	BodyFooter          string
	CommentInstructions string
	CommitTrailers      []string
	// ReportWorklog and ReportCost gate the optional sections of the comment a
	// delivery posts. Nil inherits the next layer down.
	ReportWorklog *bool
	ReportCost    *bool
	// QuoteTask says how the comment quotes the request that started the run.
	// QuoteInherit takes the next layer down.
	QuoteTask QuoteMode
}

// RepoContent is the pull-request content a repository contributes from the
// `pull_request:` block in its kvarn.yml.
//
// It carries wording and structure only. Footers, commit trailers, the report
// toggles and the quote mode have no repository layer on purpose: they carry
// run identity and operator noise control, and kvarn.yml is read from the
// branch under test, so a run could otherwise rewrite its own attribution — or
// suppress the record of what it was asked to do.
type RepoContent struct {
	TitleInstructions   string
	TitleMaxLength      *int
	BodyInstructions    string
	BodySections        []Section
	CommentInstructions string
	CommentSections     []Section
}

// Instructions is one resolved instruction field, kept split by origin so the
// prompt can say which conventions came from the operator and which from the
// repository. Organization is the operator layers joined together; Repository
// is what kvarn.yml added.
type Instructions struct {
	Organization string
	Repository   string
}

// Empty reports whether neither origin contributed anything.
func (i Instructions) Empty() bool {
	return i.Organization == "" && i.Repository == ""
}

// Content is the resolved pull-request content for one run, after every layer
// has been applied.
type Content struct {
	TitleInstructions   Instructions
	TitleMaxLength      int
	BodyInstructions    Instructions
	BodySections        []Section
	BodyFooter          string
	CommentInstructions Instructions
	CommentSections     []Section
	CommitTrailers      []string
	ReportWorklog       bool
	ReportCost          bool
	QuoteTask           QuoteMode
}

// resolveContent layers pull-request content across the operator layers and the
// repository.
//
// Instruction fields concatenate rather than override, which is the one place
// kvarn departs from last-writer-wins. Two instructions rarely conflict — "use
// Conventional Commits" and "note user-visible flag changes" stack — and
// dropping an operator convention because a repository set one word would be
// the worse failure. Everything else resolves normally: the most specific layer
// that sets a value wins.
func resolveContent(layers []PRContent, repo RepoContent) Content {
	c := Content{
		TitleMaxLength: DefaultTitleMaxLength,
		ReportWorklog:  DefaultReportWorklog,
		ReportCost:     DefaultReportCost,
		QuoteTask:      DefaultQuoteTask,
	}

	var titleOrg, bodyOrg, commentOrg []string
	for _, l := range layers {
		titleOrg = appendNonEmpty(titleOrg, l.TitleInstructions)
		bodyOrg = appendNonEmpty(bodyOrg, l.BodyInstructions)
		commentOrg = appendNonEmpty(commentOrg, l.CommentInstructions)

		if l.TitleMaxLength != nil {
			c.TitleMaxLength = *l.TitleMaxLength
		}
		if l.BodyFooter != "" {
			c.BodyFooter = l.BodyFooter
		}
		if len(l.CommitTrailers) > 0 {
			c.CommitTrailers = slicesClone(l.CommitTrailers)
		}
		if l.ReportWorklog != nil {
			c.ReportWorklog = *l.ReportWorklog
		}
		if l.ReportCost != nil {
			c.ReportCost = *l.ReportCost
		}
		if l.QuoteTask != QuoteInherit {
			c.QuoteTask = l.QuoteTask
		}
	}

	c.TitleInstructions = Instructions{
		Organization: strings.Join(titleOrg, "\n\n"),
		Repository:   strings.TrimSpace(repo.TitleInstructions),
	}
	c.BodyInstructions = Instructions{
		Organization: strings.Join(bodyOrg, "\n\n"),
		Repository:   strings.TrimSpace(repo.BodyInstructions),
	}
	c.CommentInstructions = Instructions{
		Organization: strings.Join(commentOrg, "\n\n"),
		Repository:   strings.TrimSpace(repo.CommentInstructions),
	}

	if repo.TitleMaxLength != nil {
		c.TitleMaxLength = *repo.TitleMaxLength
	}
	c.BodySections = slicesClone(repo.BodySections)
	c.CommentSections = slicesClone(repo.CommentSections)

	return c
}

// appendNonEmpty adds s to out when it holds anything but whitespace.
func appendNonEmpty(out []string, s string) []string {
	if s = strings.TrimSpace(s); s != "" {
		return append(out, s)
	}
	return out
}

// slicesClone copies a slice so a resolved Content never aliases the config it
// was built from; config stores are re-read per request and callers may retain
// what they resolve.
func slicesClone[T any](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}
