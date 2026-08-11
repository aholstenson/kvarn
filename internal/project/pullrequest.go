package project

import (
	"fmt"
	"strings"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

// maxSectionNameLen bounds a section heading so an unbounded string from a
// kvarn.yml cannot travel into every prompt and pull request a run produces.
const maxSectionNameLen = 64

// PullRequest is the `pull_request:` block: what the pull requests, commits and
// comments a run produces should say.
//
// It carries wording and structure only. Footers, commit trailers and the
// report toggles are configured by the operator in forges.toml/projects.toml
// and have no entry here: this file is read from the branch under test, so a
// run that could set them would be able to rewrite its own attribution.
type PullRequest struct {
	PullRequestContent `yaml:",inline"`
	// Modes overrides the block above for a single job mode, keyed by the name
	// a job selects with `--mode`. A review's comment and an implement run's
	// body want different things said.
	Modes map[string]PullRequestContent `yaml:"modes,omitempty"`
}

// PullRequestContent is one layer of the `pull_request:` block — the top level,
// or one entry under `modes:`.
type PullRequestContent struct {
	Title   PRTitle   `yaml:"title,omitempty"`
	Body    PRBody    `yaml:"body,omitempty"`
	Comment PRComment `yaml:"comment,omitempty"`
}

// PRTitle steers the commit subject and pull request title.
type PRTitle struct {
	Instructions string `yaml:"instructions,omitempty"`
	// MaxLength is the character budget the title is asked to fit. Unset
	// inherits the operator's value, then the compiled-in default.
	MaxLength *int `yaml:"max_length,omitempty"`
}

// PRBody steers the shared body that becomes both the commit message body and
// the pull request description.
type PRBody struct {
	Instructions string      `yaml:"instructions,omitempty"`
	Sections     []PRSection `yaml:"sections,omitempty"`
}

// PRComment steers the comment a delivery posts, including the written result
// of a read-only run.
type PRComment struct {
	Instructions string      `yaml:"instructions,omitempty"`
	Sections     []PRSection `yaml:"sections,omitempty"`
}

// PRSection is one heading the agent is asked to fill in.
type PRSection struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	// Required makes a missing section worth asking for a second time, and
	// renders it as explicitly not provided if it is still missing.
	Required bool `yaml:"required,omitempty"`
}

// Resolve merges the top-level block with the override for mode, if any, and
// converts the result to the form the forge config layers against. An empty
// mode selects the top-level block alone.
//
// Instructions concatenate — the top-level block states what every run of this
// repository should say, and the mode block adds to it. Sections merge by name:
// a mode entry replaces the top-level entry it shares a name with, in place, and
// any name the mode introduces is appended. That is what lets one mode add a
// "Testing" section without restating the whole list.
func (p PullRequest) Resolve(mode string) forgeconfig.RepoContent {
	c := p.PullRequestContent
	if m, ok := p.Modes[mode]; ok && mode != "" {
		c = mergeContent(c, m)
	}

	return forgeconfig.RepoContent{
		TitleInstructions:   strings.TrimSpace(c.Title.Instructions),
		TitleMaxLength:      c.Title.MaxLength,
		BodyInstructions:    strings.TrimSpace(c.Body.Instructions),
		BodySections:        toForgeSections(c.Body.Sections),
		CommentInstructions: strings.TrimSpace(c.Comment.Instructions),
		CommentSections:     toForgeSections(c.Comment.Sections),
	}
}

// mergeContent layers a mode's block on top of the top-level one.
func mergeContent(base, over PullRequestContent) PullRequestContent {
	out := PullRequestContent{
		Title: PRTitle{
			Instructions: joinInstructions(base.Title.Instructions, over.Title.Instructions),
			MaxLength:    base.Title.MaxLength,
		},
		Body: PRBody{
			Instructions: joinInstructions(base.Body.Instructions, over.Body.Instructions),
			Sections:     mergeSections(base.Body.Sections, over.Body.Sections),
		},
		Comment: PRComment{
			Instructions: joinInstructions(base.Comment.Instructions, over.Comment.Instructions),
			Sections:     mergeSections(base.Comment.Sections, over.Comment.Sections),
		},
	}
	if over.Title.MaxLength != nil {
		out.Title.MaxLength = over.Title.MaxLength
	}
	return out
}

// joinInstructions concatenates two instruction blocks, skipping empty ones.
func joinInstructions(base, over string) string {
	base, over = strings.TrimSpace(base), strings.TrimSpace(over)
	switch {
	case base == "":
		return over
	case over == "":
		return base
	default:
		return base + "\n\n" + over
	}
}

// mergeSections merges over into base by name, preserving base's order for the
// names it already had.
func mergeSections(base, over []PRSection) []PRSection {
	if len(over) == 0 {
		return base
	}
	out := make([]PRSection, len(base))
	copy(out, base)
	for _, s := range over {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].Name, s.Name) {
				out[i] = s
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, s)
		}
	}
	return out
}

// toForgeSections converts declared sections to the forge config form.
func toForgeSections(in []PRSection) []forgeconfig.Section {
	if len(in) == 0 {
		return nil
	}
	out := make([]forgeconfig.Section, len(in))
	for i, s := range in {
		out[i] = forgeconfig.Section{
			Name:        strings.TrimSpace(s.Name),
			Description: strings.TrimSpace(s.Description),
			Required:    s.Required,
		}
	}
	return out
}

// validate checks the block's own syntax: that every section is usable as a
// markdown heading and that mode keys name a mode. It runs at load time so a
// typo is reported against the file rather than surfacing as a pull request
// with a heading nobody meant to write.
func (p PullRequest) validate() error {
	if err := p.PullRequestContent.validate(); err != nil {
		return err
	}
	for mode, c := range p.Modes {
		if !modeNameRe.MatchString(mode) {
			return fmt.Errorf("modes: %q must be lowercase alphanumerics separated by single hyphens", mode)
		}
		if err := c.validate(); err != nil {
			return fmt.Errorf("modes.%s: %w", mode, err)
		}
	}
	return nil
}

func (c PullRequestContent) validate() error {
	if c.Title.MaxLength != nil && *c.Title.MaxLength <= 0 {
		return fmt.Errorf("title.max_length must be positive, got %d", *c.Title.MaxLength)
	}
	if err := validateSections("body.sections", c.Body.Sections); err != nil {
		return err
	}
	return validateSections("comment.sections", c.Comment.Sections)
}

// validateSections rejects headings that cannot be rendered or told apart.
func validateSections(field string, sections []PRSection) error {
	seen := make(map[string]bool, len(sections))
	for _, s := range sections {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			return fmt.Errorf("%s contains an entry with no name", field)
		}
		if len(name) > maxSectionNameLen {
			return fmt.Errorf("%s name %q is %d bytes; the limit is %d", field, name, len(name), maxSectionNameLen)
		}
		if strings.ContainsAny(name, "\r\n") {
			return fmt.Errorf("%s name %q must be a single line", field, name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("%s name %q is duplicated", field, name)
		}
		seen[key] = true
	}
	return nil
}
