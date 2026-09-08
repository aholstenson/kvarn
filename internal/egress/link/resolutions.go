package link

import (
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// resolutionTTL is how long an answered address stays attributable to the name
// it came back for. It is far longer than the 60 seconds the guest is told to
// cache, because the two answer different questions: the guest re-resolves to
// stay current, while this table only has to still know the name when the
// connection that followed the lookup arrives — and a connection pool opens
// sockets long after the resolve that seeded it.
const resolutionTTL = 30 * time.Minute

// maxResolutions caps the table. A VM that resolves more distinct addresses
// than this is talking to a CDN, where the oldest entries are the least likely
// to be dialled again.
const maxResolutions = 4096

// maxNamesPerAddress caps how many names one address is remembered under. A
// shared address answers for many, but the allowlist only has to find one of
// them, and the ones looked up most recently are the ones a connection now
// arriving is likely to be for.
const maxNamesPerAddress = 32

// resolutions remembers which hostnames the DNS forwarder handed each address
// out for. It is the only link between a connection on a port that carries no
// hostname — a database, a mail relay — and the name the allowlist is written
// in.
//
// One address may belong to several names, since shared hosting and CDNs put
// many behind one address, so an address maps to a list rather than a name.
type resolutions struct {
	mu      sync.Mutex
	entries map[string]*resolutionEntry
}

type resolutionEntry struct {
	// names are the hostnames answered with this address, most recent first.
	names []string
	seen  time.Time
}

func newResolutions() *resolutions {
	return &resolutions{entries: make(map[string]*resolutionEntry)}
}

// record notes that name was answered with ip.
func (r *resolutions) record(ip net.IP, name string) {
	if ip == nil {
		return
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "" {
		return
	}
	key := ip.String()
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	entry := r.entries[key]
	if entry == nil {
		r.evictLocked(now)
		entry = &resolutionEntry{}
		r.entries[key] = entry
	}
	entry.seen = now
	// Most recent first, so the name the guest just looked up is the one a
	// denial reports.
	entry.names = append([]string{name}, removeName(entry.names, name)...)
	if len(entry.names) > maxNamesPerAddress {
		entry.names = entry.names[:maxNamesPerAddress]
	}
}

// lookup returns the names this address was answered for, most recent first.
func (r *resolutions) lookup(ip string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry := r.entries[ip]
	if entry == nil {
		return nil
	}
	if time.Since(entry.seen) > resolutionTTL {
		delete(r.entries, ip)
		return nil
	}
	return append([]string(nil), entry.names...)
}

// evictLocked drops expired entries, and then the oldest ones if the table is
// still at its cap.
func (r *resolutions) evictLocked(now time.Time) {
	for key, entry := range r.entries {
		if now.Sub(entry.seen) > resolutionTTL {
			delete(r.entries, key)
		}
	}
	if len(r.entries) < maxResolutions {
		return
	}
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return r.entries[keys[i]].seen.Before(r.entries[keys[j]].seen)
	})
	for _, key := range keys[:len(keys)-maxResolutions+1] {
		delete(r.entries, key)
	}
}

func removeName(names []string, name string) []string {
	out := names[:0]
	for _, n := range names {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}
