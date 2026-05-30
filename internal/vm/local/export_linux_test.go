//go:build linux

package local

import "github.com/aholstenson/kvarn/internal/vm/local/vmtable"

// ScanHighestQEMUCIDFromProc is exported for unit tests. It walks procRoot
// (normally "/proc") to find the highest guest vsock CID held by running QEMU
// processes.
var ScanHighestQEMUCIDFromProc = scanHighestQEMUCIDFromProc

// ReadCIDFromCmdline is exported for unit tests. It parses a single
// /proc/<pid>/cmdline file and returns the vhost-vsock-pci guest-cid value.
var ReadCIDFromCmdline = readCIDFromCmdline

// ProcEntry mirrors the unexported procEntry so tests can construct fake
// /proc snapshots without exporting the field-level details into production
// code.
type ProcEntry struct {
	PID  int
	CID  uint32
	Comm string
}

// WalkQEMUProcs is exported for unit tests; it returns one entry per relevant
// /proc/<pid> directory.
func WalkQEMUProcs(procRoot string) []ProcEntry {
	internal := walkQEMUProcs(procRoot)
	out := make([]ProcEntry, len(internal))
	for i, e := range internal {
		out[i] = ProcEntry{PID: e.pid, CID: e.cid, Comm: e.comm}
	}
	return out
}

// HighestCID exposes the unexported highestCID helper.
func HighestCID(entries []ProcEntry) uint32 {
	internal := make([]procEntry, len(entries))
	for i, e := range entries {
		internal[i] = procEntry{pid: e.PID, cid: e.CID, comm: e.Comm}
	}
	return highestCID(internal)
}

// ReapOrphans exposes the unexported reaper so tests can drive it against a
// fake table and fake /proc snapshot.
func ReapOrphans(table *vmtable.Store, entries []ProcEntry) {
	internal := make([]procEntry, len(entries))
	for i, e := range entries {
		internal[i] = procEntry{pid: e.PID, cid: e.CID, comm: e.Comm}
	}
	reapOrphans(table, internal)
}

// NewProviderWithHighestCID creates a Provider pre-seeded as though highest is
// the largest guest CID currently in use. The next allocateCID() call will
// return highest+1. This makes the seeding arithmetic testable without hitting
// real /proc.
func NewProviderWithHighestCID(highest uint32) *Provider {
	p := &Provider{}
	if highest > 0 {
		p.nextCID.Store(highest - 2)
	}
	return p
}

// AllocateCIDForTest calls the unexported allocateCID and returns the result,
// allowing tests to observe the next CID without starting a real VM.
func (p *Provider) AllocateCIDForTest() uint32 {
	return p.allocateCID()
}
