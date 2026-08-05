package coding

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A mode steers seven independent decisions about a run, and each of them is
// its own axis below. The axes are what make a mode data rather than code: a
// repository can declare one in kvarn.yml, or a caller can supply one inline on
// StartJob, and both arrive here as a Spec.

// Workspace decides what the agent may do to the checked-out repository, which
// in turn selects the toolset it is given.
type Workspace string

const (
	// WorkspaceReadOnly withholds edit_file and write_file; the agent can still
	// read, search and run inspection commands.
	WorkspaceReadOnly Workspace = "read-only"
	// WorkspaceReadWrite gives the agent the full toolkit.
	WorkspaceReadWrite Workspace = "read-write"
)

// ValidationPolicy decides whether the project's validation steps run after the
// agent, and what a failing required step means.
type ValidationPolicy string

const (
	// ValidationSkip never runs validation.
	ValidationSkip ValidationPolicy = "skip"
	// ValidationRun runs validation. In a read-write mode a failing required
	// step is fed back to the agent to fix, and the run fails if it is still
	// red once the retry budget is spent. In a read-only mode there is nothing
	// to fix, so the outcome is recorded and the run continues.
	ValidationRun ValidationPolicy = "run"
	// ValidationRequire runs validation and fails the run as soon as a required
	// step fails, with no agent retry. It is what lets a read-only "test this
	// pull request" mode report an honest verdict instead of a green one.
	ValidationRequire ValidationPolicy = "require"
)

// Sink is one place a run's output goes.
type Sink string

const (
	// SinkNone delivers nothing; the result lives in the session event log and
	// is readable with `kvarn jobs result`.
	SinkNone Sink = "none"
	// SinkPRComment posts the agent's written result as a comment on the pull
	// request the run works on. Read-only runs can use it.
	SinkPRComment Sink = "pr-comment"
	// SinkFollowUpCommit pushes the changes onto the head branch of the pull
	// request the run started from.
	SinkFollowUpCommit Sink = "follow-up-commit"
	// SinkNewPullRequest pushes the changes to a new branch and opens a pull
	// request against the branch the run started from.
	SinkNewPullRequest Sink = "new-pull-request"
)

// StartPoint constrains where a run may begin.
type StartPoint string

const (
	// StartBranch accepts only a branch.
	StartBranch StartPoint = "branch"
	// StartPullRequest accepts only an existing pull request.
	StartPullRequest StartPoint = "pull-request"
	// StartAny accepts either.
	StartAny StartPoint = "any"
)

// ContextBlock names one section of the context pack assembled ahead of the
// task message. Blocks that have no content — no parent session to read the
// original task from, a diff the forge would not return — are left out rather
// than emitted empty.
type ContextBlock string

const (
	// ContextNone assembles no context pack at all. It is how a mode that
	// inherits blocks says it wants none: an empty list is rejected, because a
	// list that is absent and a list that is empty are the same thing once a
	// definition has crossed the wire.
	ContextNone ContextBlock = "none"
	// ContextOriginalTask is the prompt the pull request's first run was given.
	ContextOriginalTask ContextBlock = "original-task"
	// ContextPRMetadata is the pull request's title and body.
	ContextPRMetadata ContextBlock = "pr-metadata"
	// ContextPRDiff is the pull request's diff.
	ContextPRDiff ContextBlock = "pr-diff"
)

// Spec is a mode definition as written by a repository (`modes:` in kvarn.yml)
// or supplied inline on StartJob. Every axis is optional; an unset one takes
// its value from the mode named by Extends, or from the axis default.
//
// Spec is the external, serializable form. NewMode turns one into the *Mode the
// agent actually runs with.
type Spec struct {
	// Name identifies the mode on the wire and in `--mode`. For a repository
	// definition it is the key in the `modes:` map.
	Name string `json:"name,omitempty"        yaml:"name,omitempty"`
	// Description is shown by `kvarn modes list`.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Extends names the mode this one inherits from: a built-in, or another
	// mode the same repository defines. Empty means "auto".
	Extends string `json:"extends,omitempty"     yaml:"extends,omitempty"`
	// Prompt is appended to the inherited prompt body, ahead of the shared
	// project/skills/sub-agents trailer. It adds to the inherited instructions
	// rather than replacing them.
	Prompt string `json:"prompt,omitempty"      yaml:"prompt,omitempty"`

	Workspace  Workspace        `json:"workspace,omitempty"  yaml:"workspace,omitempty"`
	Validation ValidationPolicy `json:"validation,omitempty" yaml:"validation,omitempty"`
	Deliver    []Sink           `json:"deliver,omitempty"    yaml:"deliver,omitempty"`
	Start      StartPoint       `json:"start,omitempty"      yaml:"start,omitempty"`
	Context    []ContextBlock   `json:"context,omitempty"    yaml:"context,omitempty"`
}

// modeNameRe constrains a mode name to what reads well in `--mode` and in a
// metrics label: lowercase alphanumerics separated by single hyphens.
var modeNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxModeNameLen bounds a mode name so an unbounded string cannot travel from
// a kvarn.yml into every log line and error message a run produces.
const maxModeNameLen = 64

// ValidateName reports whether name is usable as a mode name. Callers that
// accept a name from outside — kvarn.yml, the wire — check it before anything
// else, so a typo is reported as a bad name rather than as a missing mode.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("mode name must not be empty")
	case len(name) > maxModeNameLen:
		return fmt.Errorf("mode name %q is %d bytes; the limit is %d", name, len(name), maxModeNameLen)
	case !modeNameRe.MatchString(name):
		return fmt.Errorf("mode name %q must be lowercase alphanumerics separated by single hyphens", name)
	}
	return nil
}

// Validate checks a Spec's own syntax: that every axis holds a value from its
// vocabulary and that the combination is coherent. It says nothing about
// whether Extends resolves — that needs the surrounding registry, and for a
// repository-defined mode the repository is not readable until the run's clone.
func (s Spec) Validate() error {
	if s.Name != "" {
		if err := ValidateName(s.Name); err != nil {
			return err
		}
	}
	if s.Extends != "" {
		if err := ValidateName(s.Extends); err != nil {
			return fmt.Errorf("extends: %w", err)
		}
	}

	switch s.Workspace {
	case "", WorkspaceReadOnly, WorkspaceReadWrite:
	default:
		return fmt.Errorf("workspace %q must be one of %s, %s", s.Workspace, WorkspaceReadOnly, WorkspaceReadWrite)
	}

	switch s.Validation {
	case "", ValidationSkip, ValidationRun, ValidationRequire:
	default:
		return fmt.Errorf("validation %q must be one of %s, %s, %s",
			s.Validation, ValidationSkip, ValidationRun, ValidationRequire)
	}

	switch s.Start {
	case "", StartBranch, StartPullRequest, StartAny:
	default:
		return fmt.Errorf("start %q must be one of %s, %s, %s", s.Start, StartBranch, StartPullRequest, StartAny)
	}

	seenSink := make(map[Sink]bool, len(s.Deliver))
	for _, sink := range s.Deliver {
		switch sink {
		case SinkNone, SinkPRComment, SinkFollowUpCommit, SinkNewPullRequest:
		default:
			return fmt.Errorf("deliver %q must be one of %s, %s, %s, %s",
				sink, SinkNone, SinkPRComment, SinkFollowUpCommit, SinkNewPullRequest)
		}
		if seenSink[sink] {
			return fmt.Errorf("deliver %q is listed twice", sink)
		}
		seenSink[sink] = true
	}
	if seenSink[SinkNone] && len(s.Deliver) > 1 {
		return fmt.Errorf("deliver %s cannot be combined with another sink", SinkNone)
	}
	if seenSink[SinkFollowUpCommit] && seenSink[SinkNewPullRequest] {
		return fmt.Errorf("deliver %s and %s are alternatives: changes land in one place or the other",
			SinkFollowUpCommit, SinkNewPullRequest)
	}

	seenBlock := make(map[ContextBlock]bool, len(s.Context))
	for _, block := range s.Context {
		switch block {
		case ContextNone, ContextOriginalTask, ContextPRMetadata, ContextPRDiff:
		default:
			return fmt.Errorf("context %q must be one of %s, %s, %s, %s",
				block, ContextNone, ContextOriginalTask, ContextPRMetadata, ContextPRDiff)
		}
		if seenBlock[block] {
			return fmt.Errorf("context %q is listed twice", block)
		}
		seenBlock[block] = true
	}
	if seenBlock[ContextNone] && len(s.Context) > 1 {
		return fmt.Errorf("context %s cannot be combined with another block", ContextNone)
	}

	// A read-only run has nothing to commit, so a sink that produces one would
	// silently never fire.
	if s.Workspace == WorkspaceReadOnly {
		for _, sink := range s.Deliver {
			if sink == SinkFollowUpCommit || sink == SinkNewPullRequest {
				return fmt.Errorf("deliver %s needs workspace %s", sink, WorkspaceReadWrite)
			}
		}
	}
	// The same for a sink that needs a pull request in a mode that can only
	// start from a branch.
	if s.Start == StartBranch {
		for _, sink := range s.Deliver {
			if sink == SinkFollowUpCommit {
				return fmt.Errorf("deliver %s needs start %s or %s", sink, StartPullRequest, StartAny)
			}
		}
	}

	return nil
}

// NewMode builds the runnable mode a Spec describes, layering it over base.
// Modes cannot be assembled field by field from outside this package — the
// prompt role and body are unexported — so this is the only way to turn a
// definition from a kvarn.yml or the wire into something an agent can run.
//
// Every axis but one carries over from the base: what a mode is for is mostly
// what the mode it extends is for, and a definition that names no start point
// inherits the base's rather than widening to "any" — a mode extending feedback
// delivers a follow-up commit, so accepting a branch start would only produce a
// run with nowhere to put its work. Validation is the exception: it is derived
// from the resolved workspace, because overriding the workspace alone should
// not leave a read-only mode running the write mode's validation policy.
func NewMode(spec Spec, base *Mode) (*Mode, error) {
	if base == nil {
		return nil, fmt.Errorf("mode %q: no base mode to extend", spec.Name)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	name := spec.Name
	if name == "" {
		return nil, fmt.Errorf("mode has no name")
	}

	m := &Mode{
		Name:        name,
		Description: spec.Description,
		BaseName:    base.BaseName,
		Workspace:   base.Workspace,
		Deliver:     slices.Clone(base.Deliver),
		Context:     slices.Clone(base.Context),
		Start:       base.Start,
		role:        base.role,
		body:        base.body,
		taskHeading: base.taskHeading,
	}
	if spec.Workspace != "" {
		m.Workspace = spec.Workspace
	}
	if spec.Deliver != nil {
		m.Deliver = slices.Clone(spec.Deliver)
	}
	if spec.Context != nil {
		// `none` is the spelling for "assemble nothing", so it becomes the empty
		// pack rather than travelling on as a block the builder would have to
		// know to ignore.
		if slices.Contains(spec.Context, ContextNone) {
			m.Context = nil
		} else {
			m.Context = slices.Clone(spec.Context)
		}
	}
	if spec.Start != "" {
		m.Start = spec.Start
	}
	if spec.Validation != "" {
		m.Validation = spec.Validation
	} else {
		m.Validation = defaultValidation(m.Workspace)
	}
	if prompt := strings.TrimSpace(spec.Prompt); prompt != "" {
		m.body = m.body + "\n\n## Additional instructions\n\n" + prompt
	}

	// The axes are checked again on the resolved mode, because inheritance can
	// produce a combination neither the spec nor the base had on its own — a
	// read-only override on top of a base that opens pull requests, say.
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("mode %q: %w", name, err)
	}
	return m, nil
}

// defaultValidation is what a mode gets when it does not say: a run that can
// change the repository is worth validating, and one that cannot is not.
func defaultValidation(ws Workspace) ValidationPolicy {
	if ws == WorkspaceReadWrite {
		return ValidationRun
	}
	return ValidationSkip
}

// validate re-checks a fully resolved mode against the same rules Spec.Validate
// applies, by describing the mode as the Spec that would produce it.
func (m *Mode) validate() error {
	return Spec{
		Workspace:  m.Workspace,
		Validation: m.Validation,
		Deliver:    m.Deliver,
		Start:      m.Start,
		Context:    m.Context,
	}.Validate()
}
