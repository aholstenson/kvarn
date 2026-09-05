// Package nixcache implements the `kvarn nix-cache` CLI: inspecting,
// clearing, and evicting the host-side Nix binary cache. These subcommands
// read the on-disk store directly, so they run on the host where the cache
// lives with no running orchestrator required.
package nixcache

import (
	"fmt"
	"os"

	nixstore "github.com/aholstenson/kvarn/internal/nixcache/store"
	"github.com/aholstenson/kvarn/internal/project"
)

// Cmd is the parent command for `kvarn nix-cache <subcommand>`.
type Cmd struct {
	Stats StatsCmd `cmd:"" help:"Show cache totals and hit/miss counters."`
	Clear ClearCmd `cmd:"" help:"Remove every cached narinfo and NAR file."`
	Evict EvictCmd `cmd:"" help:"Evict least-recently-used NAR files to satisfy a global quota."`
}

func openStore(dir string) (*nixstore.Store, error) {
	if dir != "" {
		return nixstore.New(dir), nil
	}
	d, err := nixstore.DefaultDir()
	if err != nil {
		return nil, err
	}
	return nixstore.New(d), nil
}

// StatsCmd reports cache totals.
type StatsCmd struct {
	Dir string `help:"Override nix-cache directory (default: ~/.cache/kvarn/nix-cache)." name:"dir"`
}

func (c *StatsCmd) Run() error {
	s, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	st, err := s.Stats()
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}
	fmt.Fprintf(os.Stdout, "NAR bytes:       %s (%d files)\n", formatBytes(st.NarBytes), st.NarCount)
	fmt.Fprintf(os.Stdout, "Narinfo count:   %d\n", st.NarInfoCount)
	fmt.Fprintf(os.Stdout, "NAR hits:        %d\n", st.NarHits)
	fmt.Fprintf(os.Stdout, "NAR misses:      %d\n", st.NarMisses)
	fmt.Fprintf(os.Stdout, "Narinfo hits:    %d\n", st.NarInfoHits)
	fmt.Fprintf(os.Stdout, "Narinfo misses:  %d\n", st.NarInfoMisses)
	fmt.Fprintln(os.Stdout, "Note: hit/miss counters are in-memory in the running orchestrator; this CLI reads on-disk totals only.")
	return nil
}

// ClearCmd removes every cached entry.
type ClearCmd struct {
	Dir string `help:"Override nix-cache directory (default: ~/.cache/kvarn/nix-cache)." name:"dir"`
	All bool   `help:"Confirm removal of every cached narinfo and NAR file." required:""`
}

func (c *ClearCmd) Run() error {
	s, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	if err := s.Clear(); err != nil {
		return fmt.Errorf("clear: %w", err)
	}
	fmt.Fprintln(os.Stdout, "Cleared nix cache")
	return nil
}

// EvictCmd runs a global LRU sweep.
type EvictCmd struct {
	Dir    string `help:"Override nix-cache directory (default: ~/.cache/kvarn/nix-cache)." name:"dir"`
	Global string `help:"Target total NAR size (e.g. 20G)." name:"global" required:""`
}

func (c *EvictCmd) Run() error {
	s, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	n, err := project.ParseSize(c.Global)
	if err != nil {
		return fmt.Errorf("--global: %w", err)
	}
	report, err := s.EvictGlobal(n)
	if err != nil {
		return fmt.Errorf("evict: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Evicted %d entries, freed %s\n", report.RemovedEntries, formatBytes(report.BytesFreed))
	return nil
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
