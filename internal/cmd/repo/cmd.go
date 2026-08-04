// Package repo implements the `kvarn repo` CLI: inspecting, warming and
// clearing the host-side repository mirrors. Except for `pull`, which has to
// reach the forge and therefore needs credentials, these subcommands read the
// on-disk store directly, so they run on the host where the mirrors live — no
// running orchestrator required.
package repo

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	credtoml "github.com/aholstenson/kvarn/internal/config/credential/tomlstore"
	forgetoml "github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	projtoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
	forgegithub "github.com/aholstenson/kvarn/internal/forge/github"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/scm/mirror"
)

// Cmd is the parent command for `kvarn repo <subcommand>`.
type Cmd struct {
	Pull  PullCmd  `cmd:"" help:"Fetch a project's repository into its host mirror."`
	List  ListCmd  `cmd:"" help:"List mirrored repositories."`
	GC    GCCmd    `cmd:"" name:"gc" help:"Prune unused branch refs and repack mirrors."`
	Clear ClearCmd `cmd:"" help:"Remove a project's mirror."`
}

func openStore(dir string) (*mirror.Store, error) {
	if dir == "" {
		d, err := mirror.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	return mirror.New(dir), nil
}

// PullCmd warms one project's mirror ahead of any job needing it.
type PullCmd struct {
	Project string `arg:"" help:"Project name as configured in projects.toml."`
	Branch  string `help:"Branch to fetch. Defaults to the project's default branch."`
	Depth   int    `help:"History to keep in the mirror. 0 keeps everything." default:"0"`

	Dir             string `help:"Override mirror directory (default: ~/.cache/kvarn/repos)." name:"dir"`
	ProjectsFile    string `help:"Path to projects TOML file." name:"projects-file"`
	ForgesFile      string `help:"Path to forges TOML file." name:"forges-file"`
	CredentialsFile string `help:"Path to credentials TOML file." name:"credentials-file"`
}

func (c *PullCmd) Run() error {
	ctx := context.Background()

	store, err := openStore(c.Dir)
	if err != nil {
		return err
	}

	projects := projtoml.New(pathOr(c.ProjectsFile, projtoml.DefaultPath()))
	proj, err := projects.Get(ctx, c.Project)
	if err != nil {
		return fmt.Errorf("project %q: %w", c.Project, err)
	}

	branch := c.Branch
	if branch == "" {
		branch = proj.DefaultBranch
	}
	if branch == "" {
		return fmt.Errorf("project %q has no default branch; pass --branch", c.Project)
	}

	cloneURL, creds, err := resolveForge(ctx, proj.Forge, proj.RepoURL,
		pathOr(c.ForgesFile, forgetoml.DefaultPath()),
		pathOr(c.CredentialsFile, credtoml.DefaultPath()))
	if err != nil {
		return err
	}

	depth := c.Depth
	if depth == 0 && proj.MirrorDepth != nil {
		depth = *proj.MirrorDepth
	}

	if err := store.Refresh(ctx, mirror.Ref{
		Project:     proj.Name,
		URL:         cloneURL,
		Credentials: creds,
		Depth:       depth,
	}, branch); err != nil {
		return fmt.Errorf("pull %s: %w", c.Project, err)
	}

	fmt.Fprintf(os.Stdout, "Mirrored %s (%s)\n", proj.Name, branch)
	return nil
}

// resolveForge mirrors the orchestrator's own forge resolution so the CLI
// authenticates a pull exactly the way a job would.
func resolveForge(ctx context.Context, forgeName, repoURL, forgesPath, credsPath string) (string, scm.CredentialSource, error) {
	if forgeName == "" {
		return repoURL, nil, nil
	}

	forges := forgetoml.New(forgesPath)
	cfg, err := forges.Get(ctx, forgeName)
	if err != nil {
		return "", nil, fmt.Errorf("load forge config %q: %w", forgeName, err)
	}

	impls := map[string]interface {
		ResolveCloneURL(string) (string, error)
		ResolveCredentials(context.Context, map[string]string) (scm.CredentialSource, error)
	}{
		"github": forgegithub.New(),
		"git":    forgegit.New(),
	}
	impl, ok := impls[cfg.Type]
	if !ok {
		return "", nil, fmt.Errorf("unknown forge type %q", cfg.Type)
	}

	cloneURL, err := impl.ResolveCloneURL(repoURL)
	if err != nil {
		return "", nil, fmt.Errorf("resolve clone URL: %w", err)
	}

	if cfg.Credential == "" {
		return cloneURL, nil, nil
	}
	cred, err := credtoml.New(credsPath).Get(ctx, cfg.Credential)
	if err != nil {
		return "", nil, fmt.Errorf("load credential %q: %w", cfg.Credential, err)
	}
	creds, err := impl.ResolveCredentials(ctx, cred.Config)
	if err != nil {
		return "", nil, fmt.Errorf("resolve credentials: %w", err)
	}
	return cloneURL, creds, nil
}

// ListCmd prints the mirrors on this host.
type ListCmd struct {
	Dir string `help:"Override mirror directory (default: ~/.cache/kvarn/repos)." name:"dir"`
}

func (c *ListCmd) Run() error {
	store, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	entries, err := store.List()
	if err != nil {
		return fmt.Errorf("list mirrors: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stdout, "No mirrored repositories")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tREPOSITORY\tBRANCHES\tSIZE\tLAST FETCH")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			e.Project, e.URL, len(e.Branches), formatBytes(e.SizeBytes), formatTime(e.LastFetch))
	}
	return tw.Flush()
}

// GCCmd prunes stale branch refs and repacks.
type GCCmd struct {
	Project string `arg:"" optional:"" help:"Limit to one project. Omitted collects every mirror."`

	Dir       string `help:"Override mirror directory (default: ~/.cache/kvarn/repos)." name:"dir"`
	OlderThan string `help:"Drop branch refs unused for longer than this (e.g. 720h). Empty keeps all refs." name:"older-than"`
}

func (c *GCCmd) Run() error {
	ctx := context.Background()
	store, err := openStore(c.Dir)
	if err != nil {
		return err
	}

	if c.OlderThan != "" {
		d, err := time.ParseDuration(c.OlderThan)
		if err != nil {
			return fmt.Errorf("--older-than: %w", err)
		}
		if err := store.Prune(ctx, d); err != nil {
			return fmt.Errorf("prune: %w", err)
		}
	}
	if err := store.GC(ctx, c.Project); err != nil {
		return fmt.Errorf("gc: %w", err)
	}

	fmt.Fprintln(os.Stdout, "Collected repository mirrors")
	return nil
}

// ClearCmd removes a mirror outright.
type ClearCmd struct {
	Project string `arg:"" help:"Project whose mirror should be removed."`

	Dir string `help:"Override mirror directory (default: ~/.cache/kvarn/repos)." name:"dir"`
}

func (c *ClearCmd) Run() error {
	store, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	if err := store.Remove(context.Background(), c.Project); err != nil {
		return fmt.Errorf("clear %s: %w", c.Project, err)
	}
	fmt.Fprintf(os.Stdout, "Removed mirror for %s\n", c.Project)
	return nil
}

func pathOr(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

func formatBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1fK", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
