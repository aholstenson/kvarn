// Package cache implements the `kvarn cache` CLI: inspecting, clearing, and
// evicting the host-side tool cache. These subcommands operate directly on the
// on-disk cache store, so they run on the host where that store lives — no
// running orchestrator required.
package cache

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/aholstenson/kvarn/internal/project"
	cachepkg "github.com/aholstenson/kvarn/internal/sandbox/cache"
)

// Cmd is the parent command for `kvarn cache <subcommand>`.
type Cmd struct {
	List  ListCmd  `cmd:"" help:"List cached tool layers."`
	Clear ClearCmd `cmd:"" help:"Remove cached layers for a project (or all)."`
	Evict EvictCmd `cmd:"" help:"Evict least-recently-used layers to satisfy a quota."`
}

// openCache resolves the cache store: an explicit --cache-dir, or the default
// ~/.cache/kvarn location.
func openCache(cacheDir string) (*cachepkg.FileCache, error) {
	if cacheDir != "" {
		return &cachepkg.FileCache{BaseDir: cacheDir}, nil
	}
	return cachepkg.DefaultFileCache()
}

// projectIDs returns the project directories under the cache root, or the
// single requested project.
func projectIDs(fc *cachepkg.FileCache, project string) ([]string, error) {
	if project != "" {
		return []string{project}, nil
	}
	entries, err := os.ReadDir(fc.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ListCmd prints stored cache layers.
type ListCmd struct {
	CacheDir string `help:"Override cache directory (default: ~/.cache/kvarn)." name:"cache-dir"`
	Project  string `help:"Limit to a single project ID." short:"p"`
}

func (c *ListCmd) Run() error {
	fc, err := openCache(c.CacheDir)
	if err != nil {
		return err
	}
	ids, err := projectIDs(fc, c.Project)
	if err != nil {
		return fmt.Errorf("enumerate projects: %w", err)
	}

	var rows int
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tBUCKET\tINPUT KEY\tSIZE\tLAST ACCESS\tNAMESPACE")
	for _, pid := range ids {
		entries, err := fc.List(pid)
		if err != nil {
			return fmt.Errorf("list %s: %w", pid, err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Key.Bucket < entries[j].Key.Bucket })
		for _, e := range entries {
			ns := e.Key.Namespace
			if ns == "" {
				ns = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				pid, e.Key.Bucket, shortKey(e.Key.InputKey), formatBytes(e.SizeBytes),
				e.LastAccess.Format(time.RFC3339), ns)
			rows++
		}
	}
	if rows == 0 {
		fmt.Fprintln(os.Stdout, "No cached layers")
		return nil
	}
	return tw.Flush()
}

// ClearCmd removes cached layers.
type ClearCmd struct {
	CacheDir string `help:"Override cache directory (default: ~/.cache/kvarn)." name:"cache-dir"`
	All      bool   `help:"Clear every project's cache."`
	Project  string `arg:"" optional:"" help:"Project ID to clear."`
}

func (c *ClearCmd) Run() error {
	fc, err := openCache(c.CacheDir)
	if err != nil {
		return err
	}
	if !c.All && c.Project == "" {
		return fmt.Errorf("specify a project ID or --all")
	}

	var targets []string
	if c.All {
		targets, err = projectIDs(fc, "")
		if err != nil {
			return fmt.Errorf("enumerate projects: %w", err)
		}
	} else {
		targets = []string{c.Project}
	}

	for _, pid := range targets {
		if err := fc.Clear(pid); err != nil {
			return fmt.Errorf("clear %s: %w", pid, err)
		}
		fmt.Fprintf(os.Stdout, "Cleared cache for %s\n", pid)
	}
	return nil
}

// EvictCmd runs a manual LRU sweep.
type EvictCmd struct {
	CacheDir   string `help:"Override cache directory (default: ~/.cache/kvarn)." name:"cache-dir"`
	PerProject string `help:"Per-project size limit (e.g. 5G)." name:"per-project"`
	Global     string `help:"Global size limit (e.g. 100G)." name:"global"`
}

func (c *EvictCmd) Run() error {
	fc, err := openCache(c.CacheDir)
	if err != nil {
		return err
	}
	var quota cachepkg.Quota
	if c.PerProject != "" {
		n, err := project.ParseSize(c.PerProject)
		if err != nil {
			return fmt.Errorf("--per-project: %w", err)
		}
		quota.PerProjectBytes = n
	}
	if c.Global != "" {
		n, err := project.ParseSize(c.Global)
		if err != nil {
			return fmt.Errorf("--global: %w", err)
		}
		quota.GlobalBytes = n
	}
	if quota.PerProjectBytes == 0 && quota.GlobalBytes == 0 {
		return fmt.Errorf("specify --per-project and/or --global")
	}

	report, err := fc.Evict(quota)
	if err != nil {
		return fmt.Errorf("evict: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Evicted %d entries, freed %s\n", report.RemovedEntries, formatBytes(report.BytesFreed))
	return nil
}

func shortKey(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
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
