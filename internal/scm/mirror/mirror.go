// Package mirror keeps one bare git repository per project on the host so that
// N jobs across N branches cost one fetch of the shared history rather than N
// full clones over the network.
//
// A mirror is keyed by project name, not by repository URL. Two projects can
// point at the same URL with different credentials, and hashing the URL would
// hand them a shared object store — letting a weaker-scoped project read
// objects a stronger-scoped one fetched.
//
// Job clones are taken out of the mirror over a local path, so the repository
// the VM eventually sees has no credentialed URL anywhere near it. The upstream
// URL is contacted from exactly one place, Refresh.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/observability/reqid"
	"github.com/aholstenson/kvarn/internal/scm"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"golang.org/x/sync/singleflight"
)

const (
	mirrorDirName = "mirror.git"
	metaFileName  = "meta.json"
	// atomicfile.Acquire appends ".lock", so this is the file's stem. It sits
	// beside the repository rather than inside it, so it can be taken before
	// the mirror exists and never collides with git's own *.lock files.
	lockBaseName = ".mirror"
	sourceMarker = "SOURCE"
)

// slowLockWait is how long a mirror lock may be contended before the wait is
// worth a log line. Below it the wait is noise; above it, it is the explanation
// for a job that seems to be starting slowly for no reason.
const slowLockWait = time.Second

// DefaultDir returns the standard mirror root, alongside the image and tool
// caches under the user cache directory.
func DefaultDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determine user cache dir: %w", err)
	}
	return filepath.Join(dir, "kvarn", "repos"), nil
}

// Store is a set of per-project mirrors rooted at a directory.
type Store struct {
	dir string

	// sf collapses concurrent refreshes of the same project and branch. The
	// file lock covers other processes; this covers the common case of several
	// jobs on one project starting at the same moment inside one orchestrator.
	sf singleflight.Group
}

// New constructs a Store rooted at dir.
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Dir returns the store's root directory.
func (s *Store) Dir() string { return s.dir }

// Ref identifies the upstream a project's mirror tracks.
type Ref struct {
	Project     string // mirror identity
	URL         string // upstream clone URL
	Credentials scm.CredentialSource
	// Depth bounds the history the mirror itself keeps. 0 is full history.
	Depth int
}

// CloneOpts configures a job clone taken out of a mirror.
type CloneOpts struct {
	Ref         Ref
	Branch      string
	Destination string
	// Depth bounds the history the job clone gets. 0 is full history.
	Depth int
	// WantSHA, when set, is a commit the caller already knows it needs — a
	// feedback run carries the pull request's head. If the mirror already has
	// it, the refresh touches the network not at all.
	WantSHA string
}

// Entry describes one mirror for `kvarn repo list`.
type Entry struct {
	Project   string
	URL       string
	Branches  []string
	SizeBytes int64
	LastFetch time.Time
	LastUsed  time.Time
}

// meta is the per-mirror bookkeeping that git itself does not record: which
// branches this mirror was asked for and when each was last fetched and used.
// It is the sole record of what the mirror tracks, so pruning a branch is a
// matter of dropping its ref and its entry here.
type meta struct {
	// URL identifies the upstream for a human reading `kvarn repo list`; every
	// fetch is given its URL by the caller, so this is never read back to
	// contact anything. It is stored redacted for that reason — a credentialed
	// URL would otherwise come to rest in the cache directory.
	URL      string                `json:"url"`
	Depth    int                   `json:"depth"`
	LastUsed time.Time             `json:"last_used"`
	Branches map[string]branchMeta `json:"branches"`
}

type branchMeta struct {
	SHA       string    `json:"sha"`
	LastFetch time.Time `json:"last_fetch"`
	LastUsed  time.Time `json:"last_used"`
}

func (s *Store) projectDir(project string) string {
	return filepath.Join(s.dir, sanitizeProject(project))
}

func (s *Store) mirrorPath(project string) string {
	return filepath.Join(s.projectDir(project), mirrorDirName)
}

func (s *Store) lockPath(project string) string {
	return filepath.Join(s.projectDir(project), lockBaseName)
}

// sanitizeProject maps a project name onto a single safe path component.
// Project names are operator-chosen and may contain separators.
func sanitizeProject(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "project"
	}
	return out
}

// Refresh makes sure the mirror holds branch at its current upstream tip,
// doing as little work as the situation allows.
func (s *Store) Refresh(ctx context.Context, ref Ref, branch string) error {
	return s.refresh(ctx, ref, branch, "")
}

// refresh is Refresh with the caller's already-known tip, if it has one.
func (s *Store) refresh(ctx context.Context, ref Ref, branch, wantSHA string) error {
	if ref.Project == "" {
		return errors.New("project is required")
	}
	if ref.URL == "" {
		return errors.New("repository URL is required")
	}
	if branch == "" {
		return errors.New("branch is required")
	}

	key := ref.Project + "\x00" + branch
	_, err, _ := s.sf.Do(key, func() (any, error) {
		return nil, s.refreshOnce(ctx, ref, branch, wantSHA)
	})
	return err
}

func (s *Store) refreshOnce(ctx context.Context, ref Ref, branch, wantSHA string) error {
	if err := s.ensureProjectDir(ref); err != nil {
		return err
	}

	// A mirror is a cache: if it has gone bad, throwing it away and starting
	// over is always better than failing the job that noticed.
	if err := s.refreshAttempt(ctx, ref, branch, wantSHA); err != nil {
		s.logger(ctx, ref.Project).Warn("repository mirror failed; rebuilding from scratch",
			"branch", branch, "error", err)
		if rmErr := s.removeMirror(ctx, ref.Project); rmErr != nil {
			return fmt.Errorf("discard broken mirror: %w", rmErr)
		}
		return s.refreshAttempt(ctx, ref, branch, wantSHA)
	}
	return nil
}

func (s *Store) refreshAttempt(ctx context.Context, ref Ref, branch, wantSHA string) error {
	log := s.logger(ctx, ref.Project).With("branch", branch)
	mirrorPath := s.mirrorPath(ref.Project)

	// Fast path: the commit the caller wants is already here, so there is
	// nothing to ask the network. This is what makes a feedback run free.
	if wantSHA != "" {
		lock, err := s.lock(ctx, ref.Project, false)
		if err != nil {
			return err
		}
		present := isMirror(mirrorPath) && hasCommit(ctx, mirrorPath, wantSHA)
		lock.Release()
		if present {
			log.Debug("mirror already holds the requested commit; skipping the network",
				"sha", shortSHA(wantSHA))
			return s.touch(ctx, ref, branch, wantSHA, false)
		}
	}

	// Ask the remote where the branch points. Under protocol v2 this sends a
	// ref-prefix and the server filters, so it stays one small round trip
	// however many refs the repository has.
	lsStart := time.Now()
	remoteSHA, err := s.lsRemote(ctx, ref, branch)
	if err != nil {
		return err
	}
	log.Debug("resolved the upstream branch tip",
		"sha", shortSHA(remoteSHA), "duration", logging.Elapsed(lsStart))

	lock, err := s.lock(ctx, ref.Project, false)
	if err != nil {
		return err
	}
	upToDate := isMirror(mirrorPath) && localRef(ctx, mirrorPath, branch) == remoteSHA
	lock.Release()
	if upToDate {
		log.Debug("mirror is already at the upstream tip", "sha", shortSHA(remoteSHA))
		return s.touch(ctx, ref, branch, remoteSHA, false)
	}

	exclusive, err := s.lock(ctx, ref.Project, true)
	if err != nil {
		return err
	}
	defer exclusive.Release()

	// Another process may have fetched while we waited for the lock.
	if isMirror(mirrorPath) && localRef(ctx, mirrorPath, branch) == remoteSHA {
		log.Debug("another process fetched the branch while we waited for the lock",
			"sha", shortSHA(remoteSHA))
		return s.writeTouch(ref, branch, remoteSHA, false)
	}

	fresh := !isMirror(mirrorPath)
	if fresh {
		log.Info("creating repository mirror",
			"url", gitscm.RedactURL(ref.URL), "path", mirrorPath, "depth", ref.Depth)
		if err := initMirror(ctx, mirrorPath); err != nil {
			return err
		}
	}

	// The previous tip turns a duration into something interpretable: seconds
	// for a delta on a warm mirror is a problem, seconds for the initial clone
	// of a large repository is not.
	previous := localRef(ctx, mirrorPath, branch)
	log.Info("fetching branch into the mirror",
		"to_sha", shortSHA(remoteSHA), "from_sha", shortSHA(previous), "initial", fresh)

	fetchStart := time.Now()
	if err := s.fetch(ctx, ref, branch); err != nil {
		return err
	}
	fetched := localRef(ctx, mirrorPath, branch)
	log.Info("fetched branch into the mirror",
		"sha", shortSHA(fetched), "initial", fresh, "duration", logging.Elapsed(fetchStart))

	return s.writeTouch(ref, branch, fetched, true)
}

// lsRemote resolves a branch's tip upstream without touching the mirror.
func (s *Store) lsRemote(ctx context.Context, ref Ref, branch string) (string, error) {
	auth, err := gitscm.ResolveAuth(ctx, ref.URL, ref.Credentials)
	if err != nil {
		return "", err
	}
	defer auth.Close()

	out, err := gitscm.Run(ctx, gitscm.Cmd{
		Config: auth.Config,
		Env:    auth.Env,
		Sub:    "ls-remote",
		Flags:  []string{"--heads"},
		// Both the URL and the branch pattern are caller-supplied.
		Operands: []string{ref.URL, "refs/heads/" + branch},
	})
	if err != nil {
		return "", fmt.Errorf("ls-remote %s: %w", ref.Project, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("branch %q not found in %s", branch, ref.Project)
}

// fetch brings one branch into the mirror. The refspec is given per invocation
// rather than configured as a remote: the set of branches a mirror tracks lives
// in meta.json alone, and no upstream URL is written to disk where a credential
// embedded in one could persist.
func (s *Store) fetch(ctx context.Context, ref Ref, branch string) error {
	auth, err := gitscm.ResolveAuth(ctx, ref.URL, ref.Credentials)
	if err != nil {
		return err
	}
	defer auth.Close()

	flags := []string{"--no-tags", "--prune"}
	if ref.Depth > 0 {
		flags = append(flags, "--depth", strconv.Itoa(ref.Depth))
	}

	refspec := "+refs/heads/" + branch + ":refs/heads/" + branch
	if _, err := gitscm.Run(ctx, gitscm.Cmd{
		Dir:      s.mirrorPath(ref.Project),
		Config:   auth.Config,
		Env:      auth.Env,
		Sub:      "fetch",
		Flags:    flags,
		Operands: []string{ref.URL, refspec},
	}); err != nil {
		return fmt.Errorf("fetch %s (%s): %w", ref.Project, branch, err)
	}
	return nil
}

// Clone refreshes the mirror and takes the job's working clone out of it.
func (s *Store) Clone(ctx context.Context, opts CloneOpts) error {
	if opts.Destination == "" {
		return errors.New("destination is required")
	}
	if err := s.refresh(ctx, opts.Ref, opts.Branch, opts.WantSHA); err != nil {
		return err
	}

	// A shared lock: any number of jobs may clone out of one mirror at once,
	// but none of them may be running while a fetch or a repack is.
	lock, err := s.lock(ctx, opts.Ref.Project, false)
	if err != nil {
		return err
	}
	defer lock.Release()

	log := s.logger(ctx, opts.Ref.Project).With("branch", opts.Branch)
	mirrorPath := s.mirrorPath(opts.Ref.Project)
	flags := []string{"--single-branch", "--branch", opts.Branch}
	source := mirrorPath
	if opts.Depth > 0 {
		// A plain path makes git take the local-clone shortcut, which hardlinks
		// the object store and silently ignores --depth. The file:// form goes
		// through the pack transfer instead and yields a genuinely
		// self-contained shallow repository — the same shape a network clone
		// produces, which is what the rest of the pipeline expects.
		flags = append(flags, "--no-local", "--depth", strconv.Itoa(opts.Depth))
		source = "file://" + mirrorPath
	}

	// Whether the objects are hardlinked or packed is the difference between a
	// clone that takes a moment and one that takes a while, so it is worth
	// being able to tell the two apart in a log.
	log.Debug("cloning out of the mirror",
		"destination", opts.Destination, "depth", opts.Depth, "hardlinked", opts.Depth == 0)

	cloneStart := time.Now()
	if _, err := gitscm.Run(ctx, gitscm.Cmd{
		Sub:      "clone",
		Flags:    flags,
		Operands: []string{source, opts.Destination},
	}); err != nil {
		return fmt.Errorf("clone from mirror %s: %w", opts.Ref.Project, err)
	}

	// The clone's origin points at the mirror, which does not exist inside a
	// VM. Removing it also keeps this path identical to a direct clone's.
	if err := gitscm.SanitizeClone(ctx, opts.Destination); err != nil {
		return fmt.Errorf("clone from mirror %s: %w", opts.Ref.Project, err)
	}

	log.Debug("cloned out of the mirror", "duration", logging.Elapsed(cloneStart))
	return nil
}

// RecordPush brings a branch that kvarn has just pushed upstream into the
// mirror, taking the objects from the job clone that produced them rather than
// from the network, so the next run on that branch starts warm.
//
// It declines to do anything when the job clone is shallow. Fetching from a
// shallow source would graft that shallow boundary onto the mirror, trading a
// small one-off fetch for a mirror that no longer holds the history it claims.
func (s *Store) RecordPush(ctx context.Context, ref Ref, srcRepo, branch string) error {
	log := s.logger(ctx, ref.Project).With("branch", branch)
	mirrorPath := s.mirrorPath(ref.Project)
	if !isMirror(mirrorPath) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(srcRepo, ".git", "shallow")); err == nil {
		log.Debug("not warming the mirror from a shallow clone; the next run fetches instead")
		return nil
	}

	lock, err := s.lock(ctx, ref.Project, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	refspec := "+refs/heads/" + branch + ":refs/heads/" + branch
	if _, err := gitscm.Run(ctx, gitscm.Cmd{
		Dir:      mirrorPath,
		Sub:      "fetch",
		Flags:    []string{"--no-tags"},
		Operands: []string{srcRepo, refspec},
	}); err != nil {
		return fmt.Errorf("record push on %s: %w", ref.Project, err)
	}
	sha := localRef(ctx, mirrorPath, branch)
	log.Debug("recorded the pushed branch in the mirror", "sha", shortSHA(sha))
	return s.writeTouch(ref, branch, sha, true)
}

// Prune drops branch refs that no job has asked for within retention, and the
// meta entries that track them. Objects are reclaimed by GC. A non-positive
// retention keeps every branch.
func (s *Store) Prune(ctx context.Context, retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	entries, err := s.projects()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-retention)

	for _, project := range entries {
		if err := s.pruneProject(ctx, project, cutoff); err != nil {
			s.logger(ctx, project).Warn("mirror prune failed", "error", err)
		}
	}
	return nil
}

func (s *Store) pruneProject(ctx context.Context, project string, cutoff time.Time) error {
	lock, err := s.lock(ctx, project, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	log := s.logger(ctx, project)
	m := s.readMeta(project)
	if len(m.Branches) == 0 {
		return nil
	}
	mirrorPath := s.mirrorPath(project)

	var changed bool
	for branch, bm := range m.Branches {
		if bm.LastUsed.After(cutoff) {
			continue
		}
		if _, err := gitscm.Run(ctx, gitscm.Cmd{
			Dir:      mirrorPath,
			Sub:      "update-ref",
			Flags:    []string{"-d"},
			Operands: []string{"refs/heads/" + branch},
		}); err != nil {
			log.Warn("could not drop mirrored branch", "branch", branch, "error", err)
			continue
		}
		delete(m.Branches, branch)
		changed = true
		log.Info("pruned mirrored branch", "branch", branch, "last_used", bm.LastUsed)
	}
	if !changed {
		return nil
	}
	return s.writeMeta(project, m)
}

// GC repacks a mirror and drops objects no remaining ref reaches. Passing an
// empty project collects every mirror.
func (s *Store) GC(ctx context.Context, project string) error {
	projects := []string{project}
	if project == "" {
		var err error
		if projects, err = s.projects(); err != nil {
			return err
		}
	}
	for _, p := range projects {
		if err := s.gcProject(ctx, p); err != nil {
			s.logger(ctx, p).Warn("mirror gc failed", "error", err)
		}
	}
	return nil
}

func (s *Store) gcProject(ctx context.Context, project string) error {
	mirrorPath := s.mirrorPath(project)
	if !isMirror(mirrorPath) {
		return nil
	}
	log := s.logger(ctx, project)
	// Exclusive: a repack running while a job clones out of the same mirror can
	// hand that job a pack that is being rewritten underneath it. This lock is
	// not best-effort for that reason — a failure to take it fails the sweep.
	lock, err := s.lock(ctx, project, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	// The mirror store shares a filesystem with the VM disk pool, so what a
	// repack reclaims is capacity the scheduler can admit jobs into. Measuring
	// it costs one directory walk on top of the repack's own.
	before := dirSize(mirrorPath)
	start := time.Now()

	if _, err := gitscm.Run(ctx, gitscm.Cmd{
		Dir:   mirrorPath,
		Sub:   "gc",
		Flags: []string{"--prune=now", "--quiet"},
	}); err != nil {
		return fmt.Errorf("gc %s: %w", project, err)
	}

	// Reclaiming space is a change in how much room the host has for jobs and is
	// worth saying out loud. Reclaiming none is the common case on an hourly
	// sweep, and a repack that grew the mirror only wrote the indexes git keeps
	// to make later reads cheap — neither is news.
	after := dirSize(mirrorPath)
	if after < before {
		log.Info("repacked repository mirror",
			"bytes", after, "bytes_freed", before-after, "duration", logging.Elapsed(start))
	} else {
		log.Debug("repacked repository mirror", "bytes", after, "duration", logging.Elapsed(start))
	}
	return nil
}

// Evict removes whole mirrors, least recently used first, until the store fits
// within limit. A non-positive limit disables eviction.
func (s *Store) Evict(ctx context.Context, limit int64) error {
	if limit <= 0 {
		return nil
	}
	entries, err := s.List()
	if err != nil {
		return err
	}
	var total int64
	for _, e := range entries {
		total += e.SizeBytes
	}
	if total <= limit {
		return nil
	}

	slog.Info("repository mirrors exceed their size cap; evicting least recently used",
		"bytes", total, "limit_bytes", limit, "mirrors", len(entries))

	sort.Slice(entries, func(i, j int) bool { return entries[i].LastUsed.Before(entries[j].LastUsed) })
	for _, e := range entries {
		if total <= limit {
			break
		}
		if err := s.Remove(ctx, e.Project); err != nil {
			s.logger(ctx, e.Project).Warn("mirror eviction failed", "error", err)
			continue
		}
		total -= e.SizeBytes
		s.logger(ctx, e.Project).Info("evicted repository mirror",
			"bytes_freed", e.SizeBytes, "last_used", e.LastUsed, "bytes_remaining", total)
	}
	return nil
}

// Remove deletes a project's mirror entirely.
func (s *Store) Remove(ctx context.Context, project string) error {
	lock, err := s.lock(ctx, project, true)
	if err != nil {
		return err
	}
	// Empty the mirror while still holding the lock, so nothing can be cloning
	// out of it; the lock file itself lives inside the directory, so it is
	// released before the directory goes.
	if err := os.RemoveAll(s.mirrorPath(project)); err != nil {
		lock.Release()
		return err
	}
	lock.Release()
	return os.RemoveAll(s.projectDir(project))
}

// removeMirror discards only the repository, keeping the project directory and
// its lock so a rebuild can proceed without reacquiring anything.
func (s *Store) removeMirror(ctx context.Context, project string) error {
	lock, err := s.lock(ctx, project, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	if err := os.RemoveAll(s.mirrorPath(project)); err != nil {
		return err
	}
	m := s.readMeta(project)
	m.Branches = nil
	return s.writeMeta(project, m)
}

// List reports every mirror in the store.
func (s *Store) List() ([]Entry, error) {
	projects, err := s.projects()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(projects))
	for _, p := range projects {
		m := s.readMeta(p)
		e := Entry{
			Project:   p,
			URL:       m.URL,
			SizeBytes: dirSize(s.projectDir(p)),
			LastUsed:  m.LastUsed,
		}
		for branch, bm := range m.Branches {
			e.Branches = append(e.Branches, branch)
			if bm.LastFetch.After(e.LastFetch) {
				e.LastFetch = bm.LastFetch
			}
		}
		sort.Strings(e.Branches)
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Project < entries[j].Project })
	return entries, nil
}

// projects lists the project directories present in the store. A missing store
// is an empty one.
func (s *Store) projects() ([]string, error) {
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mirror store %s: %w", s.dir, err)
	}
	var out []string
	for _, d := range dirents {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	return out, nil
}

func (s *Store) lock(ctx context.Context, project string, exclusive bool) (*atomicfile.Lock, error) {
	start := time.Now()
	lock, err := atomicfile.Acquire(ctx, s.lockPath(project), exclusive)
	if err != nil {
		return nil, fmt.Errorf("lock mirror %s: %w", project, err)
	}
	// A job blocked behind another job's fetch or a repack looks identical to a
	// slow clone from the outside; this is what tells them apart.
	if waited := time.Since(start); waited > slowLockWait {
		s.logger(ctx, project).Info("waited for the mirror lock",
			"exclusive", exclusive, "duration", waited.Round(time.Millisecond).String())
	}
	return lock, nil
}

// logger returns the context's logger tagged with the mirror being worked on,
// so every line from this package is attributable to a project without each
// call site repeating the attribute.
func (s *Store) logger(ctx context.Context, project string) *slog.Logger {
	return reqid.LoggerFrom(ctx).With("project", project)
}

// shortSHA abbreviates a commit for a log field: full hashes crowd out the rest
// of the line, and the prefix is enough to tell two tips apart or to paste into
// a `git show`. A branch the mirror does not have yet has no SHA, and comes
// back empty.
func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

func (s *Store) ensureProjectDir(ref Ref) error {
	dir := s.projectDir(ref.Project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create mirror dir: %w", err)
	}
	// A human landing in ~/.cache should be able to tell what this is, the same
	// way the tool cache marks its directories.
	marker := filepath.Join(dir, sourceMarker)
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		content := fmt.Sprintf("kvarn repository mirror for project %q (%s)\n",
			ref.Project, gitscm.RedactURL(ref.URL))
		os.WriteFile(marker, []byte(content), 0o644)
	}
	return nil
}

// touch records that a branch was used, taking the exclusive lock only for as
// long as the metadata write needs it.
func (s *Store) touch(ctx context.Context, ref Ref, branch, sha string, fetched bool) error {
	lock, err := s.lock(ctx, ref.Project, true)
	if err != nil {
		return err
	}
	defer lock.Release()
	return s.writeTouch(ref, branch, sha, fetched)
}

// writeTouch updates the metadata for one branch. The caller holds the lock.
func (s *Store) writeTouch(ref Ref, branch, sha string, fetched bool) error {
	now := time.Now()
	m := s.readMeta(ref.Project)
	m.URL = gitscm.RedactURL(ref.URL)
	m.Depth = ref.Depth
	m.LastUsed = now
	if m.Branches == nil {
		m.Branches = map[string]branchMeta{}
	}
	bm := m.Branches[branch]
	bm.SHA = sha
	bm.LastUsed = now
	if fetched {
		bm.LastFetch = now
	}
	m.Branches[branch] = bm
	return s.writeMeta(ref.Project, m)
}

func (s *Store) readMeta(project string) meta {
	var m meta
	data, err := os.ReadFile(filepath.Join(s.projectDir(project), metaFileName))
	if err != nil {
		return m
	}
	// A corrupt meta file costs a re-fetch, not a failure.
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("unreadable mirror metadata; treating the mirror as untracked",
			"project", project, "file", metaFileName, "error", err)
		return meta{}
	}
	return m
}

func (s *Store) writeMeta(project string, m meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.projectDir(project), metaFileName), append(data, '\n'), 0o644)
}

func initMirror(ctx context.Context, path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create mirror: %w", err)
	}
	if _, err := gitscm.Run(ctx, gitscm.Cmd{
		Sub:      "init",
		Flags:    []string{"--bare", "--quiet"},
		Operands: []string{path},
	}); err != nil {
		return fmt.Errorf("init mirror: %w", err)
	}
	// kvarn schedules its own gc under the exclusive lock; git's automatic one
	// would repack whenever it felt like it, including while a job clones.
	if _, err := gitscm.Run(ctx, gitscm.Cmd{
		Dir:      path,
		Sub:      "config",
		Operands: []string{"gc.auto", "0"},
	}); err != nil {
		return fmt.Errorf("configure mirror: %w", err)
	}
	return nil
}

func isMirror(path string) bool {
	info, err := os.Stat(filepath.Join(path, "HEAD"))
	return err == nil && !info.IsDir()
}

func hasCommit(ctx context.Context, mirrorPath, sha string) bool {
	_, err := gitscm.Run(ctx, gitscm.Cmd{
		Dir:      mirrorPath,
		Sub:      "cat-file",
		Flags:    []string{"-e"},
		Operands: []string{sha + "^{commit}"},
	})
	return err == nil
}

func localRef(ctx context.Context, mirrorPath, branch string) string {
	out, err := gitscm.Run(ctx, gitscm.Cmd{
		Dir:      mirrorPath,
		Sub:      "rev-parse",
		Flags:    []string{"--verify", "--quiet"},
		Operands: []string{"refs/heads/" + branch},
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
