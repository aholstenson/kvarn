package proxy

import (
	"strings"
	"sync"

	"github.com/aholstenson/kvarn/internal/nixcache/store"
)

// maxNarExpectations bounds the map of pending NAR hashes. Past it the map is
// reset rather than grown; the cost of forgetting is that a download already
// in flight is served through without being cached.
const maxNarExpectations = 100_000

// parseNarExpectation reads out of a narinfo record which NAR file it points
// at and what the bytes of that file must hash to. It reports false when the
// record does not name a NAR this handler would serve, or gives no hash in an
// algorithm the store can check.
//
// The hash has to come from the record because a NAR file name is not a
// checksum of what is served under it: cache.nixos.org names a NAR after the
// uncompressed archive and hands out a compressed file, so the two never
// agree. FileHash is the one value that describes the bytes on the wire.
func parseNarExpectation(body []byte) (name, fileHash string, ok bool) {
	var rawURL, narHash string
	for _, line := range strings.Split(string(body), "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		switch key {
		case "URL":
			rawURL = value
		case "FileHash":
			fileHash = value
		case "NarHash":
			narHash = value
		}
	}
	name, found := strings.CutPrefix(rawURL, "nar/")
	if !found {
		return "", "", false
	}
	if _, valid := store.ParseNarName(name); !valid {
		return "", "", false
	}
	if fileHash == "" {
		// An uncompressed record leaves FileHash out: the file and the archive
		// it holds are then the same bytes.
		fileHash = narHash
	}
	hash, found := strings.CutPrefix(fileHash, "sha256:")
	if !found || !store.ValidNarFileHash(hash) {
		return "", "", false
	}
	return name, hash, true
}

// narExpectations remembers what each NAR file's bytes must hash to, as taken
// from the narinfo that pointed at it. Nix asks for a record before the
// archive it describes, so the answer is in hand by the time the download
// arrives, and every record served — from upstream or from the store — puts it
// there.
type narExpectations struct {
	mu      sync.Mutex
	entries map[string]string
}

func newNarExpectations() *narExpectations {
	return &narExpectations{entries: make(map[string]string)}
}

func (e *narExpectations) put(name, fileHash string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.entries) >= maxNarExpectations {
		e.entries = make(map[string]string)
	}
	e.entries[name] = fileHash
}

// get returns the expected hash for a NAR file name, or an empty string when
// no narinfo naming it has been served.
func (e *narExpectations) get(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.entries[name]
}
