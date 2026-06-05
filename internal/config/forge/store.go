package forge

import "context"

// Compiled-in fallbacks used when neither the named forge nor the global
// [defaults] block specifies a value.
const (
	DefaultBranchPrefix      = "kvarn"
	DefaultCommitAuthorName  = "kvarn"
	DefaultCommitAuthorEmail = "kvarn@noreply"
)

// DefaultLabels returns the labels applied when neither the forge nor the
// global defaults specify any. A fresh slice is returned so callers may retain
// or mutate it without affecting other jobs.
func DefaultLabels() []string {
	return []string{"kvarn"}
}

// ForgeConfig holds the configuration for a named forge instance. Empty
// behavioral fields fall back to the global [defaults] and then the compiled-in
// constants; see ResolveBehavior.
type ForgeConfig struct {
	Name              string
	Type              string // "github", "git"
	Credential        string // references credential by name
	BranchPrefix      string
	Labels            []string
	CommitAuthorName  string
	CommitAuthorEmail string
}

// Defaults holds forge-wide default behavior applied to every forge unless the
// named forge overrides it. Empty/zero fields fall back to the compiled-in
// constants. It is the parsed form of the [defaults] block in forges.toml.
type Defaults struct {
	BranchPrefix      string
	CommitAuthorName  string
	CommitAuthorEmail string
	Labels            []string
}

// Overrides holds per-project overrides applied above the per-forge values.
// A project is more specific than the forge it points at (one forge is shared
// by many projects), so these win over everything else. Empty/zero fields fall
// through to the forge, the global defaults, and the compiled-in constants. It
// carries the relevant fields from a project config without the forge package
// depending on the project package.
type Overrides struct {
	BranchPrefix      string
	CommitAuthorName  string
	CommitAuthorEmail string
	Labels            []string
}

// Behavior is the effective forge behavior after layering: per-forge override →
// global default → compiled-in constant. Every field is populated.
type Behavior struct {
	BranchPrefix      string
	CommitAuthorName  string
	CommitAuthorEmail string
	Labels            []string
}

// ResolveBehavior layers, from lowest to highest precedence: the compiled-in
// constants, the global defaults, the per-forge values, and the per-project
// overrides. The receiver may be nil (a project without a forge), in which case
// the forge layer is skipped. Each field resolves independently; Labels are
// replaced wholesale at each layer rather than merged.
func (fc *ForgeConfig) ResolveBehavior(d Defaults, o Overrides) Behavior {
	b := Behavior{
		BranchPrefix:      DefaultBranchPrefix,
		CommitAuthorName:  DefaultCommitAuthorName,
		CommitAuthorEmail: DefaultCommitAuthorEmail,
		Labels:            DefaultLabels(),
	}

	// Global defaults override the compiled-in constants.
	if d.BranchPrefix != "" {
		b.BranchPrefix = d.BranchPrefix
	}
	if d.CommitAuthorName != "" {
		b.CommitAuthorName = d.CommitAuthorName
	}
	if d.CommitAuthorEmail != "" {
		b.CommitAuthorEmail = d.CommitAuthorEmail
	}
	if len(d.Labels) > 0 {
		b.Labels = d.Labels
	}

	// Per-forge values override the global defaults.
	if fc != nil {
		if fc.BranchPrefix != "" {
			b.BranchPrefix = fc.BranchPrefix
		}
		if fc.CommitAuthorName != "" {
			b.CommitAuthorName = fc.CommitAuthorName
		}
		if fc.CommitAuthorEmail != "" {
			b.CommitAuthorEmail = fc.CommitAuthorEmail
		}
		if len(fc.Labels) > 0 {
			b.Labels = fc.Labels
		}
	}

	// Per-project overrides take precedence over everything.
	if o.BranchPrefix != "" {
		b.BranchPrefix = o.BranchPrefix
	}
	if o.CommitAuthorName != "" {
		b.CommitAuthorName = o.CommitAuthorName
	}
	if o.CommitAuthorEmail != "" {
		b.CommitAuthorEmail = o.CommitAuthorEmail
	}
	if len(o.Labels) > 0 {
		b.Labels = o.Labels
	}

	return b
}

// Store provides CRUD operations for forge configurations. Get and Delete
// return tomlstore.ErrNotFound when no entry matches.
type Store interface {
	Get(ctx context.Context, name string) (*ForgeConfig, error)
	List(ctx context.Context) ([]*ForgeConfig, error)
	Put(ctx context.Context, fc *ForgeConfig) error
	Delete(ctx context.Context, name string) error
}

// DefaultsStore reads the forge-wide [defaults] block. A missing block or file
// is not an error: callers receive a zero-value Defaults and apply built-ins
// via ResolveBehavior.
type DefaultsStore interface {
	Defaults(ctx context.Context) (Defaults, error)
}
