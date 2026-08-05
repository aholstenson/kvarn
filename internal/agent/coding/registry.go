package coding

import (
	"fmt"
	"sort"
	"strings"
)

// Registry is the set of modes a run can choose from, keyed by name. It always
// contains the built-ins; Merge layers a repository's own definitions on top.
//
// A registry is built per run rather than held globally, because the modes a
// job may use come out of the repository it is about to work on and are only
// readable once that repository has been cloned.
type Registry map[string]*Mode

// builtinModes lists the modes kvarn ships with, in the order `kvarn modes
// list` shows them.
func builtinModes() []*Mode {
	return []*Mode{ModeAuto, ModeImplement, ModeFix, ModeFeedback, ModeReview, ModeResearch}
}

// Builtins returns a registry holding only the built-in modes.
func Builtins() Registry {
	r := make(Registry, 6)
	for _, m := range builtinModes() {
		r[m.Name] = m
	}
	return r
}

// IsBuiltin reports whether name is one of the modes kvarn ships with. It
// answers the question a caller has before a repository is readable: whether a
// mode name can be resolved right now, or only once the clone lands.
func IsBuiltin(name string) bool {
	_, ok := Builtins()[NormalizeModeName(name)]
	return ok
}

// NormalizeModeName is the single spelling rule for a mode name arriving from a
// CLI flag or the wire.
func NormalizeModeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Lookup resolves a name against the registry. Empty resolves to auto, matching
// the default a caller who named no mode asked for.
func (r Registry) Lookup(name string) (*Mode, error) {
	name = NormalizeModeName(name)
	if name == "" {
		name = ModeAuto.Name
	}
	m, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("unknown mode %q; known modes are %s", name, strings.Join(r.Names(), ", "))
	}
	return m, nil
}

// Names returns every mode name in the registry, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r))
	for name := range r {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Merge builds a registry from the built-ins plus the given definitions, which
// come from a repository's `modes:` block. Definitions may extend a built-in or
// each other in any order; a cycle, an unknown parent, or a name that shadows a
// built-in is an error rather than a mode that behaves unexpectedly at runtime.
func Merge(specs map[string]Spec) (Registry, error) {
	reg := Builtins()
	if len(specs) == 0 {
		return reg, nil
	}

	// Validate every definition before resolving any, so the error a user sees
	// is about their definition rather than about something that extends it.
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)

	pending := make(map[string]Spec, len(specs))
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
		if _, taken := reg[name]; taken {
			return nil, fmt.Errorf("mode %q is built in and cannot be redefined", name)
		}
		spec := specs[name]
		// The map key is the name; a spec that also carries one must agree.
		if spec.Name != "" && spec.Name != name {
			return nil, fmt.Errorf("mode %q declares a different name %q", name, spec.Name)
		}
		spec.Name = name
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("mode %q: %w", name, err)
		}
		pending[name] = spec
	}

	resolving := make(map[string]bool, len(pending))
	var resolve func(name string) (*Mode, error)
	resolve = func(name string) (*Mode, error) {
		if m, done := reg[name]; done {
			return m, nil
		}
		spec, ok := pending[name]
		if !ok {
			return nil, fmt.Errorf("unknown mode %q", name)
		}
		if resolving[name] {
			return nil, fmt.Errorf("mode %q extends itself through a cycle", name)
		}
		resolving[name] = true
		defer delete(resolving, name)

		parent := spec.Extends
		if parent == "" {
			parent = ModeAuto.Name
		}
		base, err := resolve(parent)
		if err != nil {
			return nil, fmt.Errorf("mode %q extends %q: %w", name, parent, err)
		}
		m, err := NewMode(spec, base)
		if err != nil {
			return nil, err
		}
		reg[name] = m
		return m, nil
	}

	for _, name := range names {
		if _, err := resolve(name); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// Resolve turns one submission's mode choice into a runnable mode. A name alone
// is looked up; a spec is built against the registry, which is what lets an
// inline definition extend a built-in or one of the repository's own modes.
func (r Registry) Resolve(name string, spec *Spec) (*Mode, error) {
	if spec == nil {
		return r.Lookup(name)
	}
	s := *spec
	if s.Name == "" {
		s.Name = NormalizeModeName(name)
	}
	if s.Name == "" {
		s.Name = InlineModeName
	}
	if _, taken := Builtins()[s.Name]; taken {
		return nil, fmt.Errorf("mode %q is built in and cannot be redefined", s.Name)
	}
	parent := s.Extends
	if parent == "" {
		parent = ModeAuto.Name
	}
	base, err := r.Lookup(parent)
	if err != nil {
		return nil, fmt.Errorf("mode %q extends %q: %w", s.Name, parent, err)
	}
	return NewMode(s, base)
}

// InlineModeName is what an inline spec that names itself nothing is called. It
// is a real name rather than an empty one because it reaches logs, session rows
// and error messages.
const InlineModeName = "inline"

// MetricsModeLabel maps a mode name to the value reported as the `mode`
// attribute on job metrics. A repository-defined or inline mode collapses to
// "custom": its name is chosen by whoever wrote it, and an unbounded attribute
// value is how a metrics backend acquires a cardinality problem.
func MetricsModeLabel(name string) string {
	name = NormalizeModeName(name)
	if name == "" {
		return ModeAuto.Name
	}
	if IsBuiltin(name) {
		return name
	}
	return "custom"
}

// ModeByName resolves a name against the built-in modes alone. It is what a
// caller with no repository in hand uses — `kvarn run`, and the submission path
// deciding whether a name is one it can already check.
func ModeByName(name string) (*Mode, error) {
	return Builtins().Lookup(name)
}
