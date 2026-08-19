// Package nixpkgs resolves a nixpkgs channel name to the commit a job installs
// from, on the host and ahead of the VM that needs it.
//
// Nix can do this resolution itself, but only from inside the guest and only
// by asking api.github.com to turn a branch name into a commit. That request
// is served by no binary cache, is the first network call a cold dependency
// install makes, and takes the whole install down with it when GitHub's API is
// unwell. Resolving on the host instead puts a cache and a fallback chain in
// front of it, and hands Nix a commit that goes straight to a tarball.
package nixpkgs

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/project"
	git "github.com/aholstenson/kvarn/internal/scm/git"
	"golang.org/x/sync/singleflight"
)

const (
	// Repo is the upstream every nixpkgs channel is a branch of.
	Repo = "https://github.com/NixOS/nixpkgs"

	// DefaultTTL is how long a resolved channel tip is reused before it is
	// looked up again. A stable channel advances a few times a week, so an
	// hour trades a little staleness for one round trip per hour per channel
	// however many jobs start in it.
	DefaultTTL = time.Hour

	// DefaultTimeout bounds a single lookup. It is short because the fallback
	// is good: a job waiting on this has a VM booted and nothing else to do,
	// and a slow forge should cost seconds, not the boot.
	DefaultTimeout = 10 * time.Second
)

// revRe matches a full commit hash, which is the only answer worth trusting:
// anything else goes into a flake reference kvarn then hands to Nix.
var revRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// LookupFunc resolves a channel to the commit its branch points at upstream.
type LookupFunc func(ctx context.Context, channel string) (string, error)

// Resolver caches channel tips for the life of the process. The zero value is
// not usable; call New.
type Resolver struct {
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time
	lookup  LookupFunc

	sf singleflight.Group

	mu     sync.Mutex
	cached map[string]entry
}

type entry struct {
	rev string
	at  time.Time
}

// Options configures a Resolver. Every field has a working default.
type Options struct {
	TTL     time.Duration
	Timeout time.Duration
	Now     func() time.Time
	Lookup  LookupFunc
}

// New builds a Resolver.
func New(opts Options) *Resolver {
	r := &Resolver{
		ttl:     opts.TTL,
		timeout: opts.Timeout,
		now:     opts.Now,
		lookup:  opts.Lookup,
		cached:  map[string]entry{},
	}
	if r.ttl == 0 {
		r.ttl = DefaultTTL
	}
	if r.timeout == 0 {
		r.timeout = DefaultTimeout
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.lookup == nil {
		r.lookup = LsRemoteTip
	}
	return r
}

// shared is the process-wide resolver. One per process is the right scope on
// both ends: the orchestrator is a single process serving every job, and a
// `kvarn local` run is a single process making one lookup.
var shared = sync.OnceValue(func() *Resolver { return New(Options{}) })

// Shared returns the process-wide resolver.
func Shared() *Resolver { return shared() }

// Rev returns the commit a channel currently points at, or "" when it could
// not be established.
//
// It never returns an error, because no answer it could give is worth failing
// a job over. A lookup that does not come back falls through, in order, to the
// last commit this process resolved for the channel however old it is, then to
// the commit compiled into the binary when the channel is the default one, and
// finally to "" — which leaves the caller holding the channel name, exactly
// where it would have been without a resolver at all.
func (r *Resolver) Rev(ctx context.Context, channel string) string {
	if channel == "" {
		return ""
	}

	if rev, ok := r.fresh(channel); ok {
		return rev
	}

	v, err, _ := r.sf.Do(channel, func() (any, error) {
		// Re-check under the singleflight: the winner of a race has just
		// stored a fresh answer that the losers should use rather than repeat.
		if rev, ok := r.fresh(channel); ok {
			return rev, nil
		}
		lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
		defer cancel()

		rev, err := r.lookup(lookupCtx, channel)
		if err != nil {
			return "", err
		}
		if !revRe.MatchString(rev) {
			return "", fmt.Errorf("nixpkgs channel %q resolved to %q, which is not a commit", channel, rev)
		}
		r.mu.Lock()
		r.cached[channel] = entry{rev: rev, at: r.now()}
		r.mu.Unlock()
		return rev, nil
	})
	if err == nil {
		return v.(string)
	}

	return r.fallback(channel, err)
}

// fresh returns a cached commit that is still inside the TTL.
func (r *Resolver) fresh(channel string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cached[channel]
	if !ok || r.now().Sub(e.at) >= r.ttl {
		return "", false
	}
	return e.rev, true
}

// fallback is what a failed lookup falls back to, best answer first.
func (r *Resolver) fallback(channel string, cause error) string {
	r.mu.Lock()
	stale, hasStale := r.cached[channel]
	r.mu.Unlock()

	switch {
	case hasStale:
		slog.Warn("could not refresh nixpkgs channel; using the last commit resolved for it",
			"channel", channel, "rev", stale.rev,
			"age", r.now().Sub(stale.at).Round(time.Second), "error", cause)
		return stale.rev
	case channel == project.DefaultNixpkgsChannel:
		slog.Warn("could not resolve nixpkgs channel; using the commit built into this binary",
			"channel", channel, "rev", project.DefaultNixpkgsRev, "error", cause)
		return project.DefaultNixpkgsRev
	default:
		slog.Warn("could not resolve nixpkgs channel; leaving Nix to resolve the branch itself",
			"channel", channel, "error", cause)
		return ""
	}
}

// LsRemoteTip asks the forge which commit a channel's branch points at. It is
// one git protocol v2 round trip against github.com, which is a different
// service from the api.github.com endpoint Nix would otherwise call.
func LsRemoteTip(ctx context.Context, channel string) (string, error) {
	out, err := git.Run(ctx, git.Cmd{
		Sub:   "ls-remote",
		Flags: []string{"--heads"},
		// The channel comes from kvarn.yml, so it is an operand.
		Operands: []string{Repo, "refs/heads/" + channel},
	})
	if err != nil {
		return "", fmt.Errorf("ls-remote nixpkgs %s: %w", channel, err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("nixpkgs has no channel branch %q", channel)
}
