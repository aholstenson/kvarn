//go:build !linux

package runner

// kvarnCredential is a no-op on non-Linux platforms.
type kvarnCredential struct{}

// lookupKvarnUser returns nil on non-Linux platforms (runner only runs in the VM).
func lookupKvarnUser() (*kvarnCredential, error) {
	return nil, nil
}

// chownIDs returns dummy IDs on non-Linux platforms (never called).
func (c *kvarnCredential) chownIDs() (int, int) {
	return 0, 0
}
