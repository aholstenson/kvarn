package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/scm"
	gitscm "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/aholstenson/kvarn/internal/scm/mirror"
)

// RepoPolicy is the resolved [repos] behavior. A zero value disables every
// piece of maintenance, which is what a Service constructed without mirrors
// gets.
type RepoPolicy struct {
	// Prefetch warms every project's default branch in the background.
	Prefetch bool
	// PrefetchInterval is how often the warm repeats.
	PrefetchInterval time.Duration
	// MirrorDepth bounds mirrored history; 0 is full history.
	MirrorDepth int
	// BranchRetention is how long an unused branch ref survives; 0 keeps them.
	BranchRetention time.Duration
	// GlobalBytes caps the whole mirror store; 0 is uncapped.
	GlobalBytes int64
}

// prefetchConcurrency bounds how many mirrors are warmed at once. Small on
// purpose: a fleet of fifty projects warming in parallel would saturate the
// host's network and disk at exactly the moment it is trying to start jobs.
const prefetchConcurrency = 2

// cloneViaMirror clones the job's working copy out of the project's host-side
// mirror, reporting whether it succeeded.
//
// A mirror is a cache, so every failure here is a warning and a false: the
// caller falls back to cloning straight from the forge. A broken cache must
// never be able to fail a job.
func (s *Service) cloneViaMirror(
	ctx context.Context,
	proj *project.Project,
	cloneURL string,
	branch string,
	cloneDir string,
	cloneDepth int,
	creds scm.CredentialSource,
	wantSHA string,
	log *slog.Logger,
) bool {
	if s.repoMirror == nil {
		return false
	}

	start := time.Now()
	if err := s.repoMirror.Clone(ctx, mirror.CloneOpts{
		Ref: mirror.Ref{
			Project:     proj.Name,
			URL:         cloneURL,
			Credentials: creds,
			Depth:       s.mirrorDepth(proj, cloneDepth, log),
		},
		Branch:      branch,
		Destination: cloneDir,
		Depth:       cloneDepth,
		WantSHA:     wantSHA,
	}); err != nil {
		if ctx.Err() != nil {
			// A cancelled or shutting-down job is not a broken mirror, and
			// reporting it as one sends whoever reads the log after an incident
			// looking at the wrong subsystem.
			log.Debug("mirror clone abandoned", "error", err)
			return false
		}
		log.Warn("repository mirror unavailable; cloning from the forge", "error", err)
		return false
	}
	log.Info("cloned from the host mirror",
		"branch", branch, "depth", cloneDepth, "duration", logging.Elapsed(start))
	return true
}

// mirrorDepth resolves how much history this project's mirror should hold.
// A mirror shallower than the job clone cannot serve it, so it is deepened to
// match rather than silently handing back a truncated history.
func (s *Service) mirrorDepth(proj *project.Project, cloneDepth int, log *slog.Logger) int {
	depth := s.repoPolicy.MirrorDepth
	if proj.MirrorDepth != nil {
		depth = *proj.MirrorDepth
	}
	if depth > 0 && (cloneDepth == 0 || cloneDepth > depth) {
		log.Info("deepening the mirror to cover the project's clone depth",
			"mirror_depth", depth, "clone_depth", cloneDepth)
		return cloneDepth
	}
	return depth
}

// prepareMergeBase brings the pull request's base branch into the job clone and
// returns the commit it sits at, so the agent can merge it inside the VM.
//
// The clone is `--single-branch`, so the base branch is simply not there
// otherwise. Nothing else has to be arranged: the transfer ships the clone's
// whole .git into the guest, so a ref fetched here arrives with it.
//
// It returns an empty SHA — never an error — when the base cannot be made
// usable. A run that cannot merge is worth continuing without the merge tools;
// a run that merges against a history it does not fully hold produces a wrong
// diff nobody would notice.
func (s *Service) prepareMergeBase(
	ctx context.Context,
	proj *project.Project,
	cloneURL string,
	cloneDir string,
	headBranch string,
	baseBranch string,
	creds scm.CredentialSource,
	log *slog.Logger,
) string {
	if baseBranch == "" || baseBranch == headBranch {
		return ""
	}
	log = log.With("base_branch", baseBranch)

	start := time.Now()
	if err := s.fetchMergeBase(ctx, proj, cloneURL, cloneDir, headBranch, baseBranch, creds); err != nil {
		log.Warn("could not prepare the base branch for merging; this run cannot merge", "error", err)
		// A half-fetched ref would travel into the guest and promise a merge
		// that would be wrong, so it goes rather than staying behind.
		if delErr := gitscm.DeleteBranch(ctx, cloneDir, baseBranch); delErr != nil {
			log.Debug("could not remove the unusable base branch ref", "error", delErr)
		}
		return ""
	}

	sha, err := gitscm.ResolveRef(ctx, cloneDir, "refs/heads/"+baseBranch)
	if err != nil {
		log.Warn("could not read the base branch tip; this run cannot merge", "error", err)
		return ""
	}
	log.Info("base branch is available for merging", "sha", sha, "duration", logging.Elapsed(start))
	return sha
}

// fetchMergeBase does the work prepareMergeBase reports on: fetch the base
// branch, then make sure the clone actually holds the commit the two branches
// parted at.
func (s *Service) fetchMergeBase(
	ctx context.Context,
	proj *project.Project,
	cloneURL string,
	cloneDir string,
	headBranch string,
	baseBranch string,
	creds scm.CredentialSource,
) error {
	ref := mirror.Ref{Project: proj.Name, URL: cloneURL, Credentials: creds}
	viaMirror := s.repoMirror != nil

	if viaMirror {
		if err := s.repoMirror.FetchInto(ctx, ref, baseBranch, cloneDir); err != nil {
			return fmt.Errorf("fetch %s from the mirror: %w", baseBranch, err)
		}
	} else if err := gitscm.FetchBranch(ctx, gitscm.FetchBranchOpts{
		RepoDir:     cloneDir,
		Source:      cloneURL,
		Branch:      baseBranch,
		Credentials: creds,
	}); err != nil {
		return err
	}

	ok, err := gitscm.HasMergeBase(ctx, cloneDir, "HEAD", "refs/heads/"+baseBranch)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	// No merge base means the clone's depth cuts above the point the branches
	// parted. Completing the history is the only way to merge correctly, and out
	// of the mirror it costs disk rather than network.
	refs := []string{"refs/heads/" + headBranch, "refs/heads/" + baseBranch}
	if viaMirror {
		err = s.repoMirror.UnshallowFrom(ctx, ref, cloneDir, refs)
	} else {
		err = gitscm.Unshallow(ctx, gitscm.UnshallowOpts{
			RepoDir:     cloneDir,
			Source:      cloneURL,
			Credentials: creds,
			Refs:        refs,
		})
	}
	if err != nil {
		return fmt.Errorf("complete history to reach the merge base: %w", err)
	}

	ok, err = gitscm.HasMergeBase(ctx, cloneDir, "HEAD", "refs/heads/"+baseBranch)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s and %s have no common ancestor in this repository", headBranch, baseBranch)
	}
	return nil
}

// recordMirrorPush brings a branch kvarn has just pushed upstream into the
// mirror, so a follow-up run on the same pull request starts warm. Best-effort:
// the next run's fetch is small either way.
func (s *Service) recordMirrorPush(ctx context.Context, proj *project.Project, cloneURL, cloneDir, branch string, log *slog.Logger) {
	if s.repoMirror == nil {
		return
	}
	ref := mirror.Ref{Project: proj.Name, URL: cloneURL}
	if err := s.repoMirror.RecordPush(ctx, ref, cloneDir, branch); err != nil {
		log.Debug("could not warm the mirror with the pushed branch", "error", err)
	}
}

// StartRepoMaintenance warms the mirrors and keeps them within their limits
// until ctx is cancelled. It is a no-op without a mirror store.
//
// Unlike session retention, the first pass runs inside the goroutine rather
// than before it: warming a fleet of projects means cloning gigabytes, and
// doing that synchronously would hold the listener closed for as long as it
// took.
func (s *Service) StartRepoMaintenance(ctx context.Context) {
	if s.repoMirror == nil {
		return
	}
	interval := s.repoPolicy.PrefetchInterval
	if !s.repoPolicy.Prefetch || interval <= 0 {
		slog.Info("repository mirror prefetch disabled; mirrors warm on first use")
		// Retention and the size cap still apply, on the same cadence a
		// prefetch would have used.
		interval = time.Hour
	}

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			s.maintainRepos(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// maintainRepos runs one maintenance pass: warm, prune, collect, evict.
func (s *Service) maintainRepos(ctx context.Context) {
	start := time.Now()
	slog.Debug("repository mirror maintenance pass starting")

	if s.repoPolicy.Prefetch {
		s.prefetchRepos(ctx)
	}
	if err := s.repoMirror.Prune(ctx, s.repoPolicy.BranchRetention); err != nil {
		slog.Warn("mirror branch prune failed", "error", err)
	}
	if err := s.repoMirror.GC(ctx, ""); err != nil {
		slog.Warn("mirror gc failed", "error", err)
	}
	if err := s.repoMirror.Evict(ctx, s.repoPolicy.GlobalBytes); err != nil {
		slog.Warn("mirror eviction failed", "error", err)
	}

	slog.Debug("repository mirror maintenance pass complete", "duration", logging.Elapsed(start))
}

// prefetchRepos refreshes every configured project's default branch.
//
// It goes through resolveForge rather than reading the project store directly
// because that is the one place a project's clone URL and credential source are
// resolved together; duplicating it here would fork the rule about which
// credential reaches which repository.
func (s *Service) prefetchRepos(ctx context.Context) {
	if s.projectStore == nil {
		return
	}
	projects, err := s.projectStore.List(ctx)
	if err != nil {
		slog.Warn("could not list projects to prefetch", "error", err)
		return
	}

	// Warming happens on a timer with no user waiting on it, so the pass as a
	// whole is what an operator wants at a glance: how many mirrors it covered,
	// how long that took, and whether any project could not be warmed. The
	// per-project detail sits at debug beneath it.
	var (
		warmed  atomic.Int64
		skipped atomic.Int64
		failed  atomic.Int64
	)
	start := time.Now()

	sem := make(chan struct{}, prefetchConcurrency)
	var wg sync.WaitGroup
	for _, proj := range projects {
		branch := proj.DefaultBranch
		if branch == "" {
			// Without a default branch there is nothing to warm; the job that
			// names one will populate the mirror itself.
			slog.Debug("no default branch to warm", "project", proj.Name)
			skipped.Add(1)
			continue
		}

		wg.Add(1)
		go func(proj *project.Project, branch string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			log := slog.With("project", proj.Name, "branch", branch)
			fr, err := s.resolveForge(ctx, proj)
			if err != nil {
				log.Warn("skipping mirror prefetch", "error", err)
				skipped.Add(1)
				return
			}
			ref := mirror.Ref{
				Project:     proj.Name,
				URL:         fr.cloneURL,
				Credentials: fr.creds,
				Depth:       s.mirrorDepth(proj, resolveCloneDepth(proj), log),
			}
			projStart := time.Now()
			if err := s.repoMirror.Refresh(ctx, ref, branch); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warn("mirror prefetch failed", "error", err)
				failed.Add(1)
				return
			}
			warmed.Add(1)
			log.Debug("mirror warmed", "duration", logging.Elapsed(projStart))
		}(proj, branch)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return
	}
	slog.Info("repository mirrors warmed",
		"warmed", warmed.Load(),
		"skipped", skipped.Load(),
		"failed", failed.Load(),
		"duration", logging.Elapsed(start))
}

// resolveCloneDepth is the depth jobs on this project will ask for, which is
// the floor for how much history its mirror has to hold.
func resolveCloneDepth(proj *project.Project) int {
	if proj.CloneDepth != nil {
		return *proj.CloneDepth
	}
	return scm.DefaultCloneDepth
}
