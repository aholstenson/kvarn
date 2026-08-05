package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

// A mode is resolved late — after the run's clone — because a project defines
// its own modes in its kvarn.yml, and that file is not readable until the
// repository is on disk. Everything before that point carries the mode as data:
// the name the submission asked for, plus the definition it supplied inline if
// it did.
//
// modeChoice is that data, together with whatever could already be settled.
// resolved is non-nil when the choice needed no repository — a built-in name,
// or an inline definition extending one — which is what lets submission reject
// an impossible request immediately instead of accepting a job that fails
// minutes later.
type modeChoice struct {
	name     string
	spec     *coding.Spec
	resolved *coding.Mode
}

// specJSON is the definition as persisted on the session, or "" when the
// submission named a mode rather than defining one.
func (c modeChoice) specJSON() (string, error) {
	if c.spec == nil {
		return "", nil
	}
	b, err := json.Marshal(c.spec)
	if err != nil {
		return "", fmt.Errorf("encode mode spec: %w", err)
	}
	return string(b), nil
}

// pushes reports whether the mode delivers changes as commits, and whether that
// is known yet. An unresolved choice answers false, false: the run may well
// push, but nothing before the clone can say so.
func (c modeChoice) pushes() (known, pushes bool) {
	if c.resolved == nil {
		return false, false
	}
	return true, c.resolved.WritesChanges()
}

// resolveSubmittedMode settles as much of a submission's mode as can be settled
// without the repository, and rejects what is already impossible.
//
// The two arms differ in how much they check. An inline definition travels with
// the request, so its syntax is checked in full and it is built outright unless
// it extends something only the repository defines. A bare name is checked only
// against the built-ins: a name that is not one of those may still be a mode
// the project defines, and refusing it here would make `modes:` unusable.
func resolveSubmittedMode(p startJobParams) (modeChoice, error) {
	name := coding.NormalizeModeName(p.mode)
	// A continuation defaults to the mode written for it. Nothing stops a
	// caller naming another one — reviewing a pull request is as reasonable as
	// revising it — so the starting point picks the default and never more.
	if name == "" && p.modeSpec == nil && p.continues() {
		name = coding.ModeFeedback.Name
	}

	if p.modeSpec == nil {
		choice := modeChoice{name: name}
		if name == "" || coding.IsBuiltin(name) {
			m, err := coding.ModeByName(name)
			if err != nil {
				return modeChoice{}, err
			}
			choice.name = m.Name
			choice.resolved = m
		}
		return choice, nil
	}

	spec := *p.modeSpec
	specName := coding.NormalizeModeName(spec.Name)
	switch {
	case specName != "" && name != "" && specName != name:
		return modeChoice{}, fmt.Errorf("mode %q and the supplied definition's name %q disagree", name, specName)
	case specName == "" && name != "":
		specName = name
	case specName == "" && name == "":
		specName = coding.InlineModeName
	}
	spec.Name = specName
	if err := spec.Validate(); err != nil {
		return modeChoice{}, err
	}

	choice := modeChoice{name: specName, spec: &spec}
	// Only a definition rooted in a built-in can be built now; one that extends
	// a project mode has to wait for the same clone that mode does.
	if spec.Extends == "" || coding.IsBuiltin(spec.Extends) {
		m, err := coding.Builtins().Resolve(specName, &spec)
		if err != nil {
			return modeChoice{}, err
		}
		choice.resolved = m
	}
	return choice, nil
}

// checkModeFeasible refuses a submission whose mode cannot do its job from
// where the run begins — either because the starting point is one the mode does
// not accept, or because the run would reach delivery with nowhere to put what
// it produced. Both are the same failure worth catching early: the alternative
// is a job that clones, boots a VM, runs an agent to completion and only then
// reports that it never had a pull request to commit onto.
//
// It applies only to a resolved mode; the same check runs again on the resolved
// mode inside the run, which is where a project-defined mode meets it — still
// before the VM boots.
func checkModeFeasible(mode *coding.Mode, continues bool) error {
	if mode == nil {
		return nil
	}
	switch {
	case mode.Start == coding.StartPullRequest && !continues:
		return fmt.Errorf("mode %q runs on an existing pull request; name one with a pull request reference", mode.Name)
	case mode.Start == coding.StartBranch && continues:
		return fmt.Errorf("mode %q runs on a branch and cannot continue a pull request", mode.Name)
	}
	if continues {
		return nil
	}

	// With no pull request to start from, a sink that acts on one has nothing to
	// act on. A comment is the exception when the mode also opens a pull
	// request, because that is the one it comments on.
	switch {
	case mode.DeliversTo(coding.SinkFollowUpCommit):
		return fmt.Errorf("mode %q delivers a follow-up commit; name the pull request to commit onto with a pull request reference", mode.Name)
	case mode.DeliversTo(coding.SinkPRComment) && !mode.DeliversTo(coding.SinkNewPullRequest):
		return fmt.Errorf("mode %q delivers a comment on a pull request; name one with a pull request reference", mode.Name)
	}
	return nil
}

// modeSpecFromProto converts the wire form of an inline definition. A request
// that carries no definition yields nil rather than an empty spec, so "named a
// mode" and "defined one" stay distinguishable all the way down.
//
// A repeated field cannot say "empty" as distinct from "absent", so a nil list
// here always means inherit and `[none]` is the only way to clear one. The
// places a human writes such a list — a kvarn.yml and a `--mode-spec` file —
// refuse the empty form for that reason, rather than letting it read as one
// thing and do the other.
func modeSpecFromProto(msg *v1.ModeSpec) *coding.Spec {
	if msg == nil {
		return nil
	}
	spec := &coding.Spec{
		Name:        msg.GetName(),
		Description: msg.GetDescription(),
		Extends:     msg.GetExtends(),
		Prompt:      msg.GetPrompt(),
		Workspace:   coding.Workspace(msg.GetWorkspace()),
		Validation:  coding.ValidationPolicy(msg.GetValidation()),
		Start:       coding.StartPoint(msg.GetStart()),
	}
	for _, sink := range msg.GetDeliver() {
		spec.Deliver = append(spec.Deliver, coding.Sink(sink))
	}
	for _, block := range msg.GetContext() {
		spec.Context = append(spec.Context, coding.ContextBlock(block))
	}
	return spec
}

// decodeModeSpec reads back the definition persisted on a session.
func decodeModeSpec(raw string) (*coding.Spec, error) {
	if raw == "" {
		return nil, nil
	}
	var spec coding.Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, fmt.Errorf("decode stored mode spec: %w", err)
	}
	return &spec, nil
}

// projectModeSpecs converts a repository's `modes:` block into the definitions
// the mode registry merges. The kvarn.yml types are plain strings — that
// package sits upstream of the coding package — and the vocabulary they hold
// was already validated when the file was read.
func projectModeSpecs(cfg *projconfig.Config) map[string]coding.Spec {
	if cfg == nil || len(cfg.Modes) == 0 {
		return nil
	}
	out := make(map[string]coding.Spec, len(cfg.Modes))
	for name, m := range cfg.Modes {
		spec := coding.Spec{
			Name:        name,
			Description: m.Description,
			Extends:     m.Extends,
			Prompt:      m.Prompt,
			Workspace:   coding.Workspace(m.Workspace),
			Validation:  coding.ValidationPolicy(m.Validation),
			Start:       coding.StartPoint(m.Start),
		}
		for _, sink := range m.Deliver {
			spec.Deliver = append(spec.Deliver, coding.Sink(sink))
		}
		for _, block := range m.Context {
			spec.Context = append(spec.Context, coding.ContextBlock(block))
		}
		out[name] = spec
	}
	return out
}

// resolveRunMode settles a run's mode against the repository it is about to
// work on: the built-ins plus whatever the project's kvarn.yml defines, with
// the submission's own inline definition layered on top when it carried one.
//
// This is where a name nobody could resolve at submission finally means
// something — or fails the run, saying which modes the project does define.
func resolveRunMode(cfg *projconfig.Config, name string, spec *coding.Spec) (*coding.Mode, error) {
	reg, err := coding.Merge(projectModeSpecs(cfg))
	if err != nil {
		return nil, fmt.Errorf("modes in kvarn.yml: %w", err)
	}
	return reg.Resolve(name, spec)
}

// invalidArgument wraps a submission-time mode error as a bad request, since
// every one of them is something the caller wrote.
func invalidArgument(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return err
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}
