// Package orchestrator defines the host-level orchestrator config file
// (orchestrator.toml). It holds operator state that doesn't fit per-project
// stores — currently the admission-pool sizing — and is read once at startup.
package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Config is the parsed orchestrator.toml. Fields are pointers (where the zero
// value is meaningful, e.g. CPUs=0) so callers can distinguish "operator set
// this explicitly" from "operator left it unset, fall through to defaults".
type Config struct {
	Scheduler Scheduler `toml:"scheduler"`
}

// Scheduler mirrors the [scheduler] table. Unset fields stay nil/empty so the
// CLI layer can apply precedence: flag > file > host detection.
type Scheduler struct {
	CPUs          *uint    `toml:"cpus,omitempty"`
	Memory        string   `toml:"memory,omitempty"`
	Disk          string   `toml:"disk,omitempty"`
	CPUOvercommit *float64 `toml:"cpu_overcommit,omitempty"`
}

// DefaultPath returns the standard orchestrator.toml location, mirroring the
// other TOML stores under ~/.config/kvarn/.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "orchestrator.toml")
}

// Load reads and parses the config at path. A missing file is not an error —
// an empty Config is returned so callers can treat absence and "all fields
// unset" identically.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}
