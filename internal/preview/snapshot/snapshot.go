// Package snapshot is the host-side store for preview state archives: the tar
// of a preview's declared state directory, kept between the moment its VM is
// stopped and the moment the next boot puts it back.
//
// It sits beside the tool caches rather than inside them on purpose. A cache
// entry is write-once, content-addressed and disposable — losing one costs a
// rebuild. A snapshot is mutable, keyed by the preview it belongs to, and
// unrecoverable: it holds whatever somebody entered into the preview. So the
// two share a root and nothing else, and this store is swept by age alone,
// never by size, because there is no quota under which throwing away a
// person's data is the right answer.
package snapshot

import (
	"errors"
	"io"
	"time"
)

// ErrNoSnapshot is returned by Open and Stat when nothing has been saved for
// the preview. It is an ordinary outcome — the first boot of every preview
// meets it — and never an error a caller should surface as a failure.
var ErrNoSnapshot = errors.New("no preview state snapshot")

// ID names one preview's archive.
//
// It is the pair the file tree is built from rather than the preview's own ID,
// because the two sides that write here disagree about what a preview is called:
// the orchestrator keys on the repository URL, `kvarn local preview` on the
// working directory. Both produce a cache.ProjectID, and both produce a
// project.RefLabel, which is already a single collision-free DNS label — so the
// tree stays human-inspectable and a ref with slashes in it is safe as a
// filename.
type ID struct {
	ProjectID string
	RefLabel  string
}

// Meta is the sidecar written beside an archive: enough to say what the archive
// is of and when, without opening it.
type Meta struct {
	// CreatedAt is when the archive was written.
	CreatedAt time.Time `json:"createdAt"`
	// Bytes is the archive's compressed size.
	Bytes int64 `json:"bytes"`
	// Commit is the commit the preview was running when the state was captured.
	// A snapshot restored onto a newer commit is the normal case, and this is
	// what says which one produced it.
	Commit string `json:"commit,omitempty"`
	// Hosts are the site hostnames the preview answered on, so an operator
	// looking at the store can tell whose data a file holds.
	Hosts []string `json:"hosts,omitempty"`
	// Ref is the unslugged git ref, kept because RefLabel is lossy.
	Ref string `json:"ref,omitempty"`
}

// PruneReport summarises one sweep.
type PruneReport struct {
	// Removed counts archives deleted, not counting the rotated generations
	// that went with them.
	Removed int
	// BytesFreed is how much disk the sweep gave back.
	BytesFreed int64
}

// Store keeps one archive per preview, plus one rotated generation of each.
type Store interface {
	// Open returns a reader over the current archive and its metadata, or
	// ErrNoSnapshot.
	Open(id ID) (io.ReadCloser, Meta, error)
	// Save writes a new archive, rotating whatever was there to the previous
	// generation. The write is temp-file-and-rename, so a reader never sees a
	// partial archive and a failure part-way leaves the old one in place.
	Save(id ID, meta Meta, r io.Reader) error
	// Touch stamps the archive as used now, which is what moves it away from the
	// prune horizon. Restoring a preview is a use.
	Touch(id ID) error
	// Stat returns an archive's metadata without opening it, or ErrNoSnapshot.
	Stat(id ID) (Meta, error)
	// Delete removes a preview's archives and metadata. A preview with nothing
	// stored is not an error: the caller wanted it gone and it is.
	Delete(id ID) error
	// Prune removes archives untouched for longer than olderThan. keep, when
	// set, protects an ID from the sweep — that is where the previews running
	// right now are excluded, since their archives are about to be rewritten.
	Prune(olderThan time.Duration, keep func(ID) bool) (PruneReport, error)
}
