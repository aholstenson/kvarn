// Package imagecache implements the `kvarn image-cache` CLI: inspecting,
// clearing, and evicting the host-side OCI image cache. These subcommands
// read the on-disk store directly, so they run on the host where the cache
// lives — no running orchestrator required.
package imagecache

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	imagestore "github.com/aholstenson/kvarn/internal/imagecache/store"
	"github.com/aholstenson/kvarn/internal/project"
)

// Cmd is the parent command for `kvarn image-cache <subcommand>`.
type Cmd struct {
	List  ListCmd  `cmd:"" help:"List cached manifests."`
	Stats StatsCmd `cmd:"" help:"Show cache totals and hit/miss counters."`
	Clear ClearCmd `cmd:"" help:"Remove cached manifests (and optionally blobs)."`
	Evict EvictCmd `cmd:"" help:"Evict least-recently-used blobs to satisfy a global quota."`
}

func openStore(dir string) (*imagestore.Store, error) {
	if dir != "" {
		return imagestore.New(dir), nil
	}
	d, err := imagestore.DefaultDir()
	if err != nil {
		return nil, err
	}
	return imagestore.New(d), nil
}

// ListCmd prints cached manifests.
type ListCmd struct {
	Dir string `help:"Override image-cache directory (default: ~/.cache/kvarn/image-cache)." name:"dir"`
}

func (c *ListCmd) Run() error {
	s, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	manifests, err := s.ListManifests()
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	if len(manifests) == 0 {
		fmt.Fprintln(os.Stdout, "No cached manifests")
		return nil
	}
	sort.Slice(manifests, func(i, j int) bool {
		if manifests[i].Registry != manifests[j].Registry {
			return manifests[i].Registry < manifests[j].Registry
		}
		if manifests[i].Name != manifests[j].Name {
			return manifests[i].Name < manifests[j].Name
		}
		return manifests[i].Ref < manifests[j].Ref
	})
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REGISTRY\tREPOSITORY\tREF\tKIND\tSIZE\tLAST ACCESS")
	for _, m := range manifests {
		kind := "digest"
		if m.IsTag {
			kind = "tag"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Registry, m.Name, m.Ref, kind, formatBytes(m.SizeBytes),
			m.LastAccess.Format(time.RFC3339))
	}
	return tw.Flush()
}

// StatsCmd reports cache totals.
type StatsCmd struct {
	Dir string `help:"Override image-cache directory (default: ~/.cache/kvarn/image-cache)." name:"dir"`
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
	fmt.Fprintf(os.Stdout, "Blob bytes:      %s (%d blobs)\n", formatBytes(st.BlobBytes), st.BlobCount)
	fmt.Fprintf(os.Stdout, "Manifest count:  %d\n", st.ManifestCount)
	fmt.Fprintf(os.Stdout, "Blob hits:       %d\n", st.BlobHits)
	fmt.Fprintf(os.Stdout, "Blob misses:     %d\n", st.BlobMisses)
	fmt.Fprintf(os.Stdout, "Manifest hits:   %d\n", st.ManifestHits)
	fmt.Fprintf(os.Stdout, "Manifest misses: %d\n", st.ManifestMiss)
	fmt.Fprintln(os.Stdout, "Note: hit/miss counters are in-memory in the running orchestrator; this CLI reads on-disk totals only.")
	return nil
}

// ClearCmd removes cached entries.
type ClearCmd struct {
	Dir  string `help:"Override image-cache directory (default: ~/.cache/kvarn/image-cache)." name:"dir"`
	All  bool   `help:"Remove every cached manifest and blob."`
	Repo string `help:"Limit clear to a single repository name (e.g. library/python)."`
}

func (c *ClearCmd) Run() error {
	s, err := openStore(c.Dir)
	if err != nil {
		return err
	}
	if !c.All && c.Repo == "" {
		return fmt.Errorf("specify --all or --repo")
	}
	if c.All {
		if err := s.Clear(); err != nil {
			return fmt.Errorf("clear: %w", err)
		}
		fmt.Fprintln(os.Stdout, "Cleared image cache")
		return nil
	}
	if err := s.ClearRepo(c.Repo); err != nil {
		return fmt.Errorf("clear repo: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Cleared manifests for %s (blobs are shared and left in place)\n", c.Repo)
	return nil
}

// EvictCmd runs a global LRU sweep.
type EvictCmd struct {
	Dir    string `help:"Override image-cache directory (default: ~/.cache/kvarn/image-cache)." name:"dir"`
	Global string `help:"Target total blob size (e.g. 50G)." name:"global" required:""`
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
