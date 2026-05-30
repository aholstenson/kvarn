// Package vmtable persists the local VM provider's host-global VM table so
// orphaned QEMU children left behind by an orchestrator crash can be reaped
// on the next startup. The table is the source of truth for which temp files
// belong to which PID; the in-memory map alone cannot survive a crash.
package vmtable

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
)

// Entry is one row in the on-disk VM table. PID + Comm together survive a
// crash and a PID-recycle: a reaper that finds a live PID whose /proc/<pid>/comm
// no longer matches the recorded Comm knows the original process is gone.
type Entry struct {
	ID        string `json:"id"`
	PID       int    `json:"pid"`
	CID       uint32 `json:"cid"`
	VsockPort uint32 `json:"vsock_port"`
	QMPSock   string `json:"qmp_sock"`
	TmpDisk   string `json:"tmp_disk"`
	TmpSeed   string `json:"tmp_seed"`
	TmpVars   string `json:"tmp_vars"`
	CreatedAt string `json:"created_at"`
	Deadline  string `json:"deadline,omitempty"`
	Comm      string `json:"comm"`
}

// Store is a JSON-backed Entry table. All mutations flush the full table via
// atomicfile.Write so a concurrent reader never observes a torn file.
type Store struct {
	path string

	mu      sync.Mutex
	entries []Entry
}

// DefaultPath returns the standard VM table location under XDG_STATE_HOME,
// falling back to ~/.config/kvarn/vms.json when no home directory is known.
func DefaultPath() string {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "kvarn", "vms.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "kvarn", "vms.json")
	}
	return filepath.Join(home, ".local", "state", "kvarn", "vms.json")
}

// Open loads the table at path. A missing file yields an empty store; a parse
// error is surfaced so the caller can decide whether to ignore it.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// List returns a snapshot copy of the entries.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Add appends e and flushes. A duplicate ID replaces the prior entry rather
// than producing two rows for the same VM.
func (s *Store) Add(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.entries {
		if existing.ID == e.ID {
			s.entries[i] = e
			return s.flushLocked()
		}
	}
	s.entries = append(s.entries, e)
	return s.flushLocked()
}

// Remove drops the entry with id and flushes. A missing id is not an error —
// reapers may race with watcher cleanup.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if e.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return s.flushLocked()
		}
	}
	return nil
}

func (s *Store) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, data, 0o600)
}
