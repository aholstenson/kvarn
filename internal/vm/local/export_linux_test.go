//go:build linux

package local

// ScanHighestQEMUCIDFromProc is exported for unit tests. It walks procRoot
// (normally "/proc") to find the highest guest vsock CID held by running QEMU
// processes.
var ScanHighestQEMUCIDFromProc = scanHighestQEMUCIDFromProc

// ReadCIDFromCmdline is exported for unit tests. It parses a single
// /proc/<pid>/cmdline file and returns the vhost-vsock-pci guest-cid value.
var ReadCIDFromCmdline = readCIDFromCmdline

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
