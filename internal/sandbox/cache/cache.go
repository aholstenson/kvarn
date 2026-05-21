package cache

import "io"

// Provider manages cached data for guest paths using tarball-based push/pull.
type Provider interface {
	// Restore returns cached data for a guest path, or nil if no cache exists.
	Restore(projectID string, guestPath string) (io.ReadCloser, error)
	// Save stores cached data for a guest path.
	Save(projectID string, guestPath string, data io.Reader) error
	// Clear removes all cached data for a project.
	Clear(projectID string) error
}
