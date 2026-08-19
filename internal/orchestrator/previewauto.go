package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	projcfg "github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/preview"
)

// Auto-start is the one path into the orchestrator that an unauthenticated
// stranger can make *create* something. Every other way to get a preview goes
// through an API key; this one is a browser asking for a hostname. What follows
// is therefore as much about what it refuses as about what it starts:
//
//   - a hostname is matched by string against the operator's own patterns, so a
//     name nothing claims costs no forge call and no database write;
//   - a matched name is resolved once even when a browser asks for twenty
//     assets at once, and the answer to "there is nothing here" is remembered
//     for a while, so a repeated request is not a repeated forge call;
//   - the whole path is rate limited, because a pull request number is a small
//     integer and an attacker can count;
//   - only an open pull request starts anything, and a fork's head needs the
//     project to have said so, since a preview runs that branch's code with the
//     project's real secrets.

// ErrAutoStartUnavailable is returned when auto-start cannot answer right now,
// as opposed to having decided there is nothing to start.
var ErrAutoStartUnavailable = errors.New("preview auto-start is busy; try again shortly")

// previewAutoStartWindow and previewAutoStartBurst bound how many hostnames may
// be resolved for the first time in one window. A preview boot is minutes of
// CPU and gigabytes of memory, so the cheap thing to limit is the step before
// it. The burst is sized for a team opening pull requests, not for a crawler
// walking the numbers.
const (
	previewAutoStartWindow = time.Minute
	previewAutoStartBurst  = 30
)

// previewDenialTTL is how long a hostname that resolved to nothing is
// remembered as such. Long enough that a browser retrying its assets does not
// reach the forge again, short enough that reopening a pull request takes
// effect while somebody is still looking at the tab.
const previewDenialTTL = time.Minute

// previewResolvedTTL is how long a hostname that resolved to a pull request is
// remembered.
//
// A hostname stops reaching the resolver once its boot claims it, but the boot
// is minutes long and the holding page polls every two seconds. Without this,
// one slow first boot with a couple of tabs open spends the whole rate-limit
// burst on re-asking the forge a question whose answer has not changed — and
// the burst is shared by every project on the host.
const previewResolvedTTL = time.Minute

// previewDenialCacheMax bounds the negative cache. It is dropped wholesale when
// full rather than evicted one by one: the entries are equal in value and
// short-lived, and the cache exists to absorb bursts, not to be complete.
const previewDenialCacheMax = 4096

// previewRefusal is a resolution failure whose reason may be shown to whoever
// asked for the hostname.
//
// Everything on this path answers an unauthenticated request, and most of what
// can go wrong names things a stranger has no business learning: the project,
// the repository a fork lives in, whatever the forge said about the
// credentials. Only the two decisions a developer needs in order to act — the
// pull request is not open, previews of forks are off here — are wrapped in
// this; everything else reaches the browser as a fixed sentence and stays in
// the log in full.
type previewRefusal struct{ reason string }

func (e *previewRefusal) Error() string { return e.reason }

// refusalReason returns the part of an error that may be shown, and whether
// there is one.
func refusalReason(err error) (string, bool) {
	var refusal *previewRefusal
	if errors.As(err, &refusal) {
		return refusal.reason, true
	}
	return "", false
}

// previewTarget is what a hostname resolved to.
type previewTarget struct {
	Project string
	// Ref is the git ref to boot — a pull request's head branch.
	Ref string
	// PR is the pull request, spelled the way the forge spells one.
	PR string
	// Fork says the pull request's head lives in a fork rather than in the
	// project's own repository. Resolving the hostname is the only place that is
	// known, and it decides whether the preview may ever keep state.
	Fork bool
}

// previewHostResolver answers what an unclaimed hostname should boot. It is a
// function rather than a method so the manager's gate — single-flight, negative
// cache, rate limit — can be exercised without a project store or a forge.
type previewHostResolver func(ctx context.Context, host string) (previewTarget, error)

// autoStarter guards the resolver. Its whole job is to make sure one hostname
// costs one lookup.
type autoStarter struct {
	resolve previewHostResolver
	now     func() time.Time
	log     *slog.Logger

	mu sync.Mutex
	// inflight is the single-flight: the first request for a hostname resolves
	// it and the rest of the burst wait on that answer.
	inflight map[string]*autoStartCall
	// denied remembers hostnames that resolved to nothing, with the reason, so
	// the answer can be repeated without asking the forge again.
	denied map[string]autoStartDenial
	// resolved remembers hostnames that named an open pull request, for the same
	// reason and over a shorter horizon.
	resolved map[string]autoStartResolution
	// windowStart and count are the rate limit over first-time resolutions.
	windowStart time.Time
	count       int
}

// autoStartCall is one in-flight resolution others can wait on.
type autoStartCall struct {
	done   chan struct{}
	target previewTarget
	err    error
}

// autoStartDenial is a remembered refusal.
type autoStartDenial struct {
	err   error
	until time.Time
}

// autoStartResolution is a remembered answer.
type autoStartResolution struct {
	target previewTarget
	until  time.Time
}

func newAutoStarter(resolve previewHostResolver, now func() time.Time) *autoStarter {
	return &autoStarter{
		resolve:  resolve,
		now:      now,
		log:      slog.With("component", "preview-auto-start"),
		inflight: make(map[string]*autoStartCall),
		denied:   make(map[string]autoStartDenial),
		resolved: make(map[string]autoStartResolution),
	}
}

// Resolve answers what a hostname should boot, at most once per hostname at a
// time.
func (a *autoStarter) Resolve(ctx context.Context, host string) (previewTarget, error) {
	a.mu.Lock()
	if denial, ok := a.denied[host]; ok {
		if a.now().Before(denial.until) {
			a.mu.Unlock()
			return previewTarget{}, denial.err
		}
		delete(a.denied, host)
	}
	if hit, ok := a.resolved[host]; ok {
		if a.now().Before(hit.until) {
			a.mu.Unlock()
			return hit.target, nil
		}
		delete(a.resolved, host)
	}
	if call, ok := a.inflight[host]; ok {
		a.mu.Unlock()
		select {
		case <-call.done:
			return call.target, call.err
		case <-ctx.Done():
			return previewTarget{}, ctx.Err()
		}
	}
	if !a.allowLocked() {
		a.mu.Unlock()
		return previewTarget{}, ErrAutoStartUnavailable
	}
	call := &autoStartCall{done: make(chan struct{})}
	a.inflight[host] = call
	a.mu.Unlock()

	// The resolution outlives the request that started it for the same reason a
	// boot does: a browser that gives up must not cancel the forge lookup the
	// rest of the burst is waiting on.
	call.target, call.err = a.resolve(context.WithoutCancel(ctx), host)

	a.mu.Lock()
	delete(a.inflight, host)
	if call.err != nil {
		a.denyLocked(host, call.err)
	} else {
		a.rememberLocked(host, call.target)
	}
	a.mu.Unlock()
	close(call.done)

	return call.target, call.err
}

// allowLocked takes one token from the current window, starting a new one when
// the old has passed.
func (a *autoStarter) allowLocked() bool {
	now := a.now()
	if now.Sub(a.windowStart) >= previewAutoStartWindow {
		a.windowStart = now
		a.count = 0
	}
	if a.count >= previewAutoStartBurst {
		return false
	}
	a.count++
	return true
}

// denyLocked remembers a refusal. A refusal that is only about right now —
// a forge that could not be reached, a rate limit — is remembered too, since
// repeating it immediately would not produce a better answer either.
func (a *autoStarter) denyLocked(host string, err error) {
	if len(a.denied) >= previewDenialCacheMax {
		a.denied = make(map[string]autoStartDenial, previewDenialCacheMax)
	}
	a.denied[host] = autoStartDenial{err: err, until: a.now().Add(previewDenialTTL)}
}

// rememberLocked keeps an answer for the burst of requests that follows it. The
// cache is bounded and dropped wholesale for the same reason the denials are.
func (a *autoStarter) rememberLocked(host string, target previewTarget) {
	if len(a.resolved) >= previewDenialCacheMax {
		a.resolved = make(map[string]autoStartResolution, previewDenialCacheMax)
	}
	a.resolved[host] = autoStartResolution{target: target, until: a.now().Add(previewResolvedTTL)}
}

// Forget drops what a hostname resolved to, refusal or answer, so an explicit
// start does not have to wait out a stale "nothing here" and a pull request
// that just closed is not started again from a cached answer.
func (a *autoStarter) Forget(host string) {
	a.mu.Lock()
	delete(a.denied, host)
	delete(a.resolved, host)
	a.mu.Unlock()
}

// AutoStart resolves a hostname nothing claims and registers the preview it
// names, so the caller can boot it like any other. It reports preview.ErrNoRoute
// when no project claims the name, which is the ordinary case for a typo and is
// not worth logging as a failure.
func (m *previewManager) AutoStart(ctx context.Context, host string) (*preview.Preview, error) {
	if !m.enabled() || m.auto == nil {
		return nil, preview.ErrNoRoute
	}
	host = preview.NormalizeHost(host)

	target, err := m.auto.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	p, err := m.Register(ctx, target.Project, target.Ref, previewOrigin{
		PR:            target.PR,
		AutoStartHost: host,
		Fork:          target.Fork,
	})
	if err != nil {
		return nil, err
	}
	m.log.Info("preview auto-started", "preview", p.ID, "host", host, "pr", target.PR)
	return p, nil
}

// previewPRSweepInterval is how often auto-started previews are checked against
// their pull requests.
const previewPRSweepInterval = 10 * time.Minute

// previewPRState reads the state of a project's pull request — "open",
// "closed" or "merged".
type previewPRState func(ctx context.Context, project, pr string) (string, error)

// ReapClosedPullRequests forgets auto-started previews whose pull request is no
// longer open.
//
// Idle reaping already stops their VMs, so this is not about resources. It is
// about the record and the hostname it holds: a preview nobody registered
// should not need anybody to unregister it, and a name left claimed by a merged
// branch is a name the next pull request cannot have.
//
// A preview an operator started by hand is never touched, whether or not it
// knows a pull request. Somebody asked for that one, and only somebody can say
// it is finished.
func (m *previewManager) ReapClosedPullRequests(ctx context.Context) {
	if !m.enabled() || m.prState == nil {
		return
	}

	previews, err := m.store.List(ctx)
	if err != nil {
		m.log.Warn("could not list previews to check their pull requests", "error", err)
		return
	}

	for _, p := range previews {
		if !p.AutoStarted() || p.PR == "" {
			continue
		}
		state, err := m.prState(ctx, p.Project, p.PR)
		if err != nil {
			// A forge that cannot be reached is not evidence the pull request
			// closed, and removing a preview on that reading would take down
			// something people are using.
			m.log.Debug("could not read a preview's pull request",
				"preview", p.ID, "pr", p.PR, "error", err)
			continue
		}
		if state == "open" {
			continue
		}
		if err := m.Remove(ctx, p.ID); err != nil {
			m.log.Warn("could not remove the preview of a closed pull request",
				"preview", p.ID, "pr", p.PR, "error", err)
			continue
		}
		m.log.Info("removed the preview of a pull request that is no longer open",
			"preview", p.ID, "pr", p.PR, "pr_state", state)
		if m.auto != nil {
			// The hostname is free again, and the next request for it should
			// resolve rather than be answered from a refusal remembered while
			// the record still existed.
			m.auto.Forget(p.AutoStartHost)
		}
	}
}

// previewPRState is the Service's half of that sweep.
func (s *Service) previewPRState(ctx context.Context, projectName, pr string) (string, error) {
	if s.projectStore == nil {
		return "", errors.New("no project store")
	}
	proj, err := s.projectStore.Get(ctx, projectName)
	if err != nil {
		return "", fmt.Errorf("load project %q: %w", projectName, err)
	}
	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		return "", fmt.Errorf("resolve forge: %w", err)
	}
	if fr.impl == nil {
		return "", fmt.Errorf("project %q has no forge configured", projectName)
	}
	details, err := fr.impl.GetPullRequest(ctx, forge.GetPROpts{
		RepoURL:     fr.cloneURL,
		PRRef:       pr,
		Credentials: fr.creds,
	})
	if err != nil {
		return "", fmt.Errorf("read pull request %s: %w", pr, err)
	}
	return details.State, nil
}

// resolvePreviewHost is the Service's half of auto-start: match the hostname
// against the configured routes, then ask the forge what the pull request it
// names is.
func (s *Service) resolvePreviewHost(ctx context.Context, host string) (previewTarget, error) {
	router, err := s.previewRouter(ctx)
	if err != nil {
		return previewTarget{}, err
	}
	match, err := router.Match(host)
	if err != nil {
		return previewTarget{}, err
	}

	proj, err := s.projectStore.Get(ctx, match.Project)
	if err != nil {
		return previewTarget{}, fmt.Errorf("load project %q: %w", match.Project, err)
	}

	fr, err := s.resolveForge(ctx, proj)
	if err != nil {
		return previewTarget{}, fmt.Errorf("resolve forge: %w", err)
	}
	if fr.impl == nil {
		return previewTarget{}, fmt.Errorf(
			"project %q has no forge configured, so there is nothing to ask which branch pull request %s is",
			proj.Name, match.PR)
	}

	pr, err := fr.impl.GetPullRequest(ctx, forge.GetPROpts{
		RepoURL:     fr.cloneURL,
		PRRef:       match.PR,
		Credentials: fr.creds,
	})
	if err != nil {
		return previewTarget{}, fmt.Errorf("read pull request %s: %w", match.PR, err)
	}
	if pr.State != "open" {
		return previewTarget{}, &previewRefusal{
			reason: fmt.Sprintf("pull request %s is %s", match.PR, pr.State),
		}
	}
	if isForkPR(pr) && !previewAllowsForks(proj) {
		// A fork's head is written by somebody without push access, and a
		// preview would run it with this project's resolved secrets. Off unless
		// the operator has said otherwise for this project. Which fork it is
		// goes to the log rather than to the browser.
		slog.Info("refusing to preview a fork's pull request",
			"project", proj.Name, "pr", match.PR, "head_repo", pr.HeadRepo)
		return previewTarget{}, &previewRefusal{
			reason: fmt.Sprintf(
				"pull request %s comes from a fork, and previews of forks are not enabled for this project",
				match.PR),
		}
	}
	if pr.HeadBranch == "" {
		return previewTarget{}, fmt.Errorf("pull request %s does not name a head branch", match.PR)
	}

	return previewTarget{Project: proj.Name, Ref: pr.HeadBranch, PR: match.PR, Fork: isForkPR(pr)}, nil
}

// previewRouter compiles every project's auto-start patterns.
//
// It is rebuilt per resolution rather than cached: projects.toml is edited
// while the orchestrator runs, and a route table that needed a restart to pick
// up a new project would be a worse surprise than the string work of building
// one. Only a hostname that no running preview already claims gets here, and
// the negative cache in front means an unclaimed one gets here rarely.
func (s *Service) previewRouter(ctx context.Context) (*preview.Router, error) {
	if s.projectStore == nil {
		return nil, preview.ErrNoRoute
	}
	projects, err := s.projectStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	var routes []preview.Route
	for _, proj := range projects {
		if len(proj.Preview.AutoStart) == 0 {
			continue
		}
		domain, err := s.previewDomain(proj)
		if err != nil {
			// Previews off for this project, or no domain to form names under.
			// Either way it claims nothing.
			continue
		}
		for _, pattern := range proj.Preview.AutoStart {
			route, err := preview.CompileRoute(proj.Name, pattern, domain)
			if err != nil {
				// One unusable pattern must not take the other projects' routes
				// with it, so it is reported and skipped.
				slog.Warn("ignoring preview auto_start pattern",
					"project", proj.Name, "pattern", pattern, "error", err)
				continue
			}
			routes = append(routes, route)
		}
	}
	if len(routes) == 0 {
		return nil, preview.ErrNoRoute
	}
	return preview.NewRouter(routes)
}

// previewAllowsForks reports whether this project may preview a fork's head.
func previewAllowsForks(proj *projcfg.Project) bool {
	return proj.Preview.AllowForks != nil && *proj.Preview.AllowForks
}
