package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// Previews are the first thing the orchestrator holds that is neither a job nor
// a piece of config. A job is dispatched, runs and ends; a preview is *asked
// for*, boots when somebody wants it, and is stopped when nobody does. That
// inversion is what shapes this file:
//
//   - the durable record says a preview exists and what it is called, while
//     everything a running preview owns (VM, sandbox, lease) lives only in
//     memory and is rebuilt after a restart;
//   - capacity is taken with TryAcquire rather than Acquire, because an HTTP
//     request is waiting on the answer and queueing behind an hour of jobs is
//     not an answer;
//   - a boot is single-flighted per preview, or the burst of asset requests a
//     browser makes on first load would each start a VM.

// ErrPreviewsDisabled is returned when previews are asked for on a host that
// has not configured them.
var ErrPreviewsDisabled = errors.New("preview environments are not configured on this host")

// ErrAtCapacity is returned when a preview cannot boot because the host is full
// and nothing idle could be evicted to make room.
var ErrAtCapacity = errors.New("no capacity for another preview environment")

// ErrPreviewDraining is returned when a boot is refused because the host is
// draining. Previews cannot migrate, so a draining host stops the ones it has
// and starts no more.
var ErrPreviewDraining = errors.New("host is draining; not starting preview environments")

// previewReapInterval is how often idle and max-lifetime reaping runs. Previews
// are measured in minutes and hours, so a coarse tick is enough and keeps an
// idle host quiet.
const previewReapInterval = 30 * time.Second

// previewBootRetryDelay is how long a failed boot is left alone before an
// unauthenticated request may start another one.
//
// A preview whose boot fails before it claims its hostnames never enters the
// host index, so every following request for that name resolves it afresh and
// would start the same doomed boot again — a clone and a VM per page load, each
// of which can evict a healthy preview to make room. The record is where the
// failure is remembered, and this is how long it is trusted. An explicit start
// ignores it: somebody who just fixed the kvarn.yml should not have to wait.
const previewBootRetryDelay = 2 * time.Minute

// defaultPreviewStateTimeout bounds one preview's state capture when the
// operator has not said otherwise. It has to fit a database dump and a tar of a
// data directory, and it is what a drain waits on per preview, so it is
// generous rather than open-ended.
const defaultPreviewStateTimeout = 2 * time.Minute

// PreviewPolicy is the resolved [preview] configuration. A zero Domain disables
// previews entirely.
type PreviewPolicy struct {
	// Domain is the base domain preview hostnames are formed under.
	Domain string
	// IdleTimeout stops a preview that has served no request for this long.
	// Zero never reaps on idle.
	IdleTimeout time.Duration
	// MaxLifetime stops a preview this long after it booted, whatever its
	// traffic. Zero disables the cap.
	MaxLifetime time.Duration
	// MaxConcurrent bounds how many previews run at once. Zero is unbounded.
	MaxConcurrent int
	// MaxMemoryBytes and MaxDiskBytes cap what one preview VM may request,
	// below whatever its kvarn.yml asks for. Zero leaves the request alone.
	MaxMemoryBytes uint64
	MaxDiskBytes   int64
	// StateTimeout bounds how long one preview's state capture may take. It is
	// what keeps a drain or a shutdown from waiting on a repository whose save
	// hook never returns.
	StateTimeout time.Duration
	// StateRetention drops state archives untouched for this long. Zero never
	// prunes.
	StateRetention time.Duration
	// MaxStateBytes is the operator's ceiling on one preview's archive, over
	// whatever the repository's own max_size asks for. Zero leaves the
	// repository's answer alone.
	MaxStateBytes int64
}

// Enabled reports whether previews are configured at all.
func (p PreviewPolicy) Enabled() bool { return p.Domain != "" }

// PreviewSandbox is what a booted preview hands back to the manager: a way into
// the guest, and a way to tear the whole thing down. Everything else the boot
// needed — the runner, the shell session, the serve processes — is the boot's
// own business and does not outlive it as a separate handle.
type PreviewSandbox interface {
	// DialGuest opens a connection to a port inside the VM.
	DialGuest(ctx context.Context, port uint16) (net.Conn, error)
	// Close destroys the VM, taking its serve processes with it.
	Close()

	// The rest is what a graceful stop needs in order to leave something behind.
	// A capture has to shut the servers down, run the repository's save hook in
	// the boot's own shell, and tar the result out — all while the VM is still
	// up, which is why these outlive the boot rather than ending with it.

	// BareRunner talks straight to the VM rather than through whatever container
	// a step wrapped itself in, which is where the tar has to run.
	BareRunner() sandbox.RunnerProxy
	// GetRunner and GetShellSessionID are the shell the save hook runs in, with
	// the preview's environment still exported into it.
	GetRunner() sandbox.RunnerProxy
	GetShellSessionID() string
	// Processes supervises the serve steps, and is how they are asked to stop
	// before their data is captured.
	Processes() sandbox.ProcessRunner
}

// previewBootSandbox is the wider surface the boot itself drives. It is a
// superset of PreviewSandbox: once the boot returns, only the two methods above
// are still needed, and narrowing the type the manager holds keeps the manager
// testable against something far smaller than a sandbox session.
type previewBootSandbox interface {
	PreviewSandbox

	CanDialGuest() bool
	GetWorkingDir() string
	GetBaseCommit() string
	RunSetup(ctx context.Context, cfg *projconfig.Config, onDone sandbox.OnStepDone, onOutput sandbox.OnOutput) (*sandbox.SetupResult, error)
}

// PreviewSandboxFactory creates the sandbox a preview runs in. It is separate
// from SandboxFactory because a preview needs ingress and long-lived processes,
// which a job never asks for.
type PreviewSandboxFactory func(ctx context.Context, opts sandbox.Opts) (previewBootSandbox, error)

// defaultPreviewSandboxFactory boots a real sandbox; *sandbox.Session satisfies
// previewBootSandbox through its own methods.
func defaultPreviewSandboxFactory(ctx context.Context, opts sandbox.Opts) (previewBootSandbox, error) {
	return sandbox.Start(ctx, opts)
}

// previewInstance is a running preview's in-memory half.
type previewInstance struct {
	sandbox   PreviewSandbox
	sites     []preview.Site
	sessionID string
	// lease is the capacity the VM holds; released when the preview stops.
	lease scheduler.Lease
	// startedAt is when the VM came up, for the max-lifetime cap.
	startedAt time.Time

	// cfg, snapshotID and commit are what capturing this preview's state needs
	// and only its boot knows: which servers to stop and which state to keep,
	// where the archive belongs, and which commit produced it.
	cfg        *projconfig.Config
	snapshotID snapshot.ID
	commit     string

	closeOnce sync.Once
}

// close releases everything the instance holds. Idempotent, because a preview
// can be stopped by an explicit request, by the reaper, and by drain at once.
func (i *previewInstance) close() {
	i.closeOnce.Do(func() {
		if i.sandbox != nil {
			i.sandbox.Close()
		}
		if i.lease != nil {
			i.lease.Release()
		}
	})
}

// state is what the repository declared about keeping state, tolerant of an
// instance built by a spec that supplied no config.
func (i *previewInstance) state() projconfig.PreviewState {
	if i.cfg == nil {
		return projconfig.PreviewState{}
	}
	return i.cfg.Preview.State
}

// previewBoot is the result of provisioning one preview.
type previewBoot struct {
	Sandbox   PreviewSandbox
	Sites     []preview.Site
	SessionID string
	Lease     scheduler.Lease
	// Config is the kvarn.yml the preview came up on, kept because stopping it
	// needs to know which servers to shut down and what state to keep.
	Config *projconfig.Config
	// SnapshotID names this preview's state archive on the host.
	SnapshotID snapshot.ID
	// Commit is what the workspace was checked out at, recorded on whatever
	// archive this boot produces.
	Commit string
}

// previewBooter provisions a preview: clone, read kvarn.yml, take capacity,
// boot the VM, run setup, start the serve processes and wait for the ready
// checks. It is a function rather than a method so the manager's own logic —
// capacity, eviction, reaping, drain — can be exercised without a VM.
//
// logs receives the output of the preview's serve processes for as long as they
// run, which is well past the boot's return.
type previewBooter func(ctx context.Context, p *preview.Preview, logs *preview.LogBuffer) (*previewBoot, error)

// previewManager owns the running previews and the policy around them.
type previewManager struct {
	store  preview.Store
	policy PreviewPolicy
	boot   previewBooter
	now    func() time.Time
	log    *slog.Logger
	// auto turns a hostname nothing claims into a preview to boot. Nil when no
	// project has auto-start configured, or on a manager built without one, in
	// which case an unclaimed hostname is simply not found.
	auto *autoStarter
	// prState reads whether a pull request is still open, which is what decides
	// that an auto-started preview has outlived its reason to exist. Nil leaves
	// those previews to the ordinary idle and lifetime reaping.
	prState previewPRState
	// snapshots is where a stopped preview's declared state is kept. Nil means
	// previews here hold nothing between boots: every stop is a discard and
	// every boot comes up fresh.
	snapshots snapshot.Store
	// snapshotIDs says which archive a preview's state belongs to. It is a
	// function because the answer is derived from the project's repository URL,
	// which lives in the project store rather than on the preview record.
	snapshotIDs func(ctx context.Context, p *preview.Preview) (snapshot.ID, error)

	mu sync.Mutex
	// onInstanceGone is called after a preview's in-memory half is taken down,
	// so anything holding connections into that VM can let go of them. Ingress
	// pools connections across requests and would otherwise keep a stopped
	// preview's bridge streams open until the pool expired them.
	onInstanceGone func()
	// live holds the in-memory half of every preview currently holding
	// resources, keyed by preview ID.
	live map[string]*previewInstance
	// booting is the singleflight: one entry per preview with a boot in
	// progress, so a burst of requests joins one boot instead of racing.
	booting map[string]*previewBootCall
	// logs are the per-preview ring buffers. They outlive an individual boot so
	// `kvarn preview logs` can still explain a preview that just crashed.
	logs     map[string]*preview.LogBuffer
	draining bool
}

// previewBootCall is one in-flight boot others can wait on.
type previewBootCall struct {
	done chan struct{}
	err  error
}

// newPreviewManager builds a manager. A nil store or a policy with no domain
// yields a manager that reports previews as disabled.
func newPreviewManager(store preview.Store, policy PreviewPolicy, boot previewBooter) *previewManager {
	return &previewManager{
		store:   store,
		policy:  policy,
		boot:    boot,
		now:     time.Now,
		log:     slog.With("component", "preview"),
		live:    make(map[string]*previewInstance),
		booting: make(map[string]*previewBootCall),
		logs:    make(map[string]*preview.LogBuffer),
	}
}

// enabled reports whether this manager can do anything at all.
func (m *previewManager) enabled() bool {
	return m != nil && m.store != nil && m.policy.Enabled() && m.boot != nil
}

// Get returns the durable record for a preview.
func (m *previewManager) Get(ctx context.Context, id string) (*preview.Preview, error) {
	if !m.enabled() {
		return nil, ErrPreviewsDisabled
	}
	return m.store.Get(ctx, id)
}

// FindByHost returns the preview a hostname routes to.
func (m *previewManager) FindByHost(ctx context.Context, host string) (*preview.Preview, error) {
	if !m.enabled() {
		return nil, ErrPreviewsDisabled
	}
	return m.store.FindByHost(ctx, host)
}

// List returns every preview.
func (m *previewManager) List(ctx context.Context) ([]*preview.Preview, error) {
	if !m.enabled() {
		return nil, ErrPreviewsDisabled
	}
	return m.store.List(ctx)
}

// Logs returns the retained tail of a preview's process output.
func (m *previewManager) Logs(id string, lines int) string {
	if !m.enabled() {
		return ""
	}
	m.mu.Lock()
	buf := m.logs[id]
	m.mu.Unlock()
	if buf == nil {
		return ""
	}
	return buf.Tail(lines)
}

// logBuffer returns the preview's ring buffer, creating it on first use.
func (m *previewManager) logBuffer(id string) *preview.LogBuffer {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := m.logs[id]
	if buf == nil {
		buf = preview.NewLogBuffer(preview.DefaultLogCapacity)
		m.logs[id] = buf
	}
	return buf
}

// Touch stamps a preview's last-request time. Called by ingress on every
// request, so it does the cheapest write the store offers.
func (m *previewManager) Touch(ctx context.Context, id string) {
	if !m.enabled() {
		return
	}
	if err := m.store.TouchRequest(ctx, id, m.now().UTC()); err != nil {
		m.log.Warn("could not stamp preview request time", "preview", id, "error", err)
	}
}

// previewOrigin is what the caller of Register knows about why the preview
// exists. Both fields are empty for a preview an operator asked for by name.
type previewOrigin struct {
	// PR is the pull request the preview is of.
	PR string
	// AutoStartHost is the hostname whose request brought it into being.
	AutoStartHost string
	// Fork says the pull request's head lives in a fork. It is only ever known
	// on the auto-start path, which is the only path that asks the forge; an
	// explicit `kvarn preview up` names a ref in the project's own repository
	// and leaves it false.
	Fork bool
}

// Register creates or refreshes the durable record for a preview of a ref,
// without booting it. It is what `kvarn preview up` calls first: the row (and
// with it the hostname) has to exist before anything can route to the preview.
//
// A preview that already exists keeps its identity and gains whatever the
// caller knows that it does not: a branch registered by hand and then reached
// through its pull request's hostname is one preview, not two, and the second
// route is what teaches the first which pull request it belongs to.
//
// What it never gains is a different provenance. AutoStartHost says "nobody
// registered this, so nobody has to unregister it", which is what the closed
// pull request sweep acts on. Letting a request stamp it onto a preview an
// operator started by hand would hand that preview's lifetime to whoever merges
// the pull request that happens to share its branch.
func (m *previewManager) Register(ctx context.Context, project, ref string, origin previewOrigin) (*preview.Preview, error) {
	if !m.enabled() {
		return nil, ErrPreviewsDisabled
	}

	id := preview.ID(project, ref)
	now := m.now().UTC()

	existing, err := m.store.Get(ctx, id)
	if err != nil && !errors.Is(err, preview.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		changed := false
		if origin.PR != "" && existing.PR != origin.PR {
			existing.PR = origin.PR
			changed = true
		}
		if origin.AutoStartHost != "" && existing.AutoStarted() &&
			existing.AutoStartHost != origin.AutoStartHost {
			existing.AutoStartHost = origin.AutoStartHost
			changed = true
		}
		// Fork only ever goes on. Learning that a branch is a fork's is a
		// restriction, and the caller that does not know — an explicit start —
		// must not be able to lift one that a hostname resolution established.
		if origin.Fork && !existing.Fork {
			existing.Fork = true
			changed = true
		}
		if !changed {
			return existing, nil
		}
		existing.UpdatedAt = now
		if err := m.store.Put(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	p := &preview.Preview{
		ID:            id,
		Project:       project,
		Ref:           ref,
		PR:            origin.PR,
		AutoStartHost: origin.AutoStartHost,
		Fork:          origin.Fork,
		State:         preview.StateStopped,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := m.store.Put(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// Ensure makes sure a preview is running, starting a boot if it is not.
//
// It returns as soon as the preview's state is known rather than waiting for a
// boot to finish: a first boot takes a minute or more, and the caller — ingress
// or the CLI — has a better answer for the person waiting than a held
// connection. Callers that want to wait watch the boot's session instead.
//
// A boot that failed a moment ago is not repeated. Ingress reaches this on
// every request for an unclaimed hostname, and a preview that cannot come up at
// all — no `preview:` block, a clone that fails — would otherwise cost a fresh
// clone and VM per page load.
func (m *previewManager) Ensure(ctx context.Context, id string) (*preview.Preview, error) {
	return m.ensure(ctx, id, false)
}

// EnsureNow is Ensure for an explicit start, which retries a failed boot
// immediately. Somebody who just corrected the thing that broke it is the whole
// reason the backoff has an exception.
func (m *previewManager) EnsureNow(ctx context.Context, id string) (*preview.Preview, error) {
	return m.ensure(ctx, id, true)
}

func (m *previewManager) ensure(ctx context.Context, id string, force bool) (*preview.Preview, error) {
	if !m.enabled() {
		return nil, ErrPreviewsDisabled
	}

	p, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return p, ErrPreviewDraining
	}
	_, isLive := m.live[id]
	_, isBooting := m.booting[id]
	m.mu.Unlock()

	if isLive && p.State == preview.StateRunning {
		return p, nil
	}
	if isBooting {
		return p, nil
	}
	// A preview whose state is still being written out is neither running nor
	// ready to boot. Booting one now would restore the archive the capture is
	// about to replace, so the caller gets the record and the holding page keeps
	// polling until the capture finishes and the row says stopped.
	if p.State == preview.StateStopping {
		return p, nil
	}
	if !force && p.State == preview.StateFailed &&
		m.now().UTC().Sub(p.UpdatedAt) < previewBootRetryDelay {
		return p, nil
	}

	if err := m.startBoot(ctx, id); err != nil {
		return p, err
	}

	// Re-read so the caller sees the booting state startBoot wrote, rather than
	// the stopped one it was handed a moment ago.
	if updated, err := m.store.Get(ctx, id); err == nil {
		return updated, nil
	}
	return p, nil
}

// startBoot marks the preview as booting and launches the boot, unless one is
// already in flight.
//
// The state is written here, before the goroutine starts, rather than inside
// it. A caller that returns while the record still says "stopped" hands back a
// preview that looks like nothing is happening to it, and anything waiting for
// the boot to settle — `preview up --wait`, the holding page's first poll —
// would conclude it already had.
func (m *previewManager) startBoot(ctx context.Context, id string) error {
	m.mu.Lock()
	if m.draining {
		m.mu.Unlock()
		return ErrPreviewDraining
	}
	if _, exists := m.booting[id]; exists {
		m.mu.Unlock()
		return nil
	}
	call := &previewBootCall{done: make(chan struct{})}
	m.booting[id] = call
	m.mu.Unlock()

	unclaim := func() {
		m.mu.Lock()
		delete(m.booting, id)
		m.mu.Unlock()
		close(call.done)
	}

	p, err := m.store.Get(ctx, id)
	if err != nil {
		unclaim()
		return err
	}

	logs := m.logBuffer(id)
	logs.Reset()

	now := m.now().UTC()
	p.State = preview.StateBooting
	p.Error = ""
	p.UpdatedAt = now
	p.LastRequestAt = now
	if err := m.store.Put(ctx, p); err != nil {
		unclaim()
		return err
	}

	// The boot itself outlives the request that asked for it: an HTTP client
	// that gives up after five seconds must not cancel a VM that is halfway up.
	go func() {
		call.err = m.runBoot(context.Background(), p, logs)
		unclaim()
	}()
	return nil
}

// runBoot provisions a preview, evicting an idle one if the host is full. The
// preview is already marked booting by the time this runs.
func (m *previewManager) runBoot(ctx context.Context, p *preview.Preview, logs *preview.LogBuffer) error {
	id := p.ID

	// The concurrency bound is the manager's, not the scheduler's: a preview
	// that fits the capacity pool can still be one preview too many.
	if err := m.makeRoom(ctx, id); err != nil {
		return m.recordBootFailure(ctx, p, err)
	}

	boot, err := m.boot(ctx, p, logs)
	if errors.Is(err, scheduler.ErrWouldBlock) {
		// The pool is full of previews and jobs. Give up the least-recently
		// wanted idle preview and try once more; a second refusal means the
		// host is genuinely busy and the caller gets the holding page.
		if evicted := m.evictIdle(ctx, id); evicted != "" {
			m.log.Info("evicted an idle preview to make room", "evicted", evicted, "for", id)
			boot, err = m.boot(ctx, p, logs)
		}
	}
	if err != nil {
		if errors.Is(err, scheduler.ErrWouldBlock) {
			err = ErrAtCapacity
		}
		return m.recordBootFailure(ctx, p, err)
	}

	instance := &previewInstance{
		sandbox:    boot.Sandbox,
		sites:      boot.Sites,
		sessionID:  boot.SessionID,
		lease:      boot.Lease,
		startedAt:  m.now().UTC(),
		cfg:        boot.Config,
		snapshotID: boot.SnapshotID,
		commit:     boot.Commit,
	}

	m.mu.Lock()
	if m.draining {
		// The host started draining while this was coming up. Nothing routes to
		// it and nothing will, so tear it down rather than leave a VM holding a
		// lease nobody can reach.
		m.mu.Unlock()
		instance.close()
		return m.recordBootFailure(ctx, p, ErrPreviewDraining)
	}
	// A previous instance under the same ID should not exist — the singleflight
	// prevents it — but closing it before overwriting is what keeps a bug there
	// from leaking a VM rather than merely double-booting one.
	if old := m.live[id]; old != nil {
		old.close()
	}
	m.live[id] = instance
	m.mu.Unlock()

	now := m.now().UTC()
	p.State = preview.StateRunning
	p.Sites = boot.Sites
	p.SessionID = boot.SessionID
	p.Error = ""
	p.StartedAt = instance.startedAt
	p.UpdatedAt = now
	if p.LastRequestAt.IsZero() {
		p.LastRequestAt = now
	}
	p.ExpiresAt = time.Time{}
	if m.policy.MaxLifetime > 0 {
		p.ExpiresAt = instance.startedAt.Add(m.policy.MaxLifetime)
	}
	if err := m.store.Put(ctx, p); err != nil {
		// The VM is up but unaddressable, which is worse than no VM: it holds a
		// lease and nothing can route to it.
		m.stopInstance(id)
		return fmt.Errorf("record running preview: %w", err)
	}

	m.log.Info("preview running", "preview", id, "hosts", p.Hosts())
	return nil
}

// recordBootFailure marks a preview failed and returns the error unchanged, so
// callers can both report and log it.
func (m *previewManager) recordBootFailure(ctx context.Context, p *preview.Preview, cause error) error {
	now := m.now().UTC()
	p.State = preview.StateFailed
	p.Error = cause.Error()
	// SessionID is left as the boot set it: the failed boot's session is the
	// only place the reason is written out in full.
	p.StartedAt = time.Time{}
	p.ExpiresAt = time.Time{}
	p.UpdatedAt = now
	if err := m.store.Put(ctx, p); err != nil {
		m.log.Error("could not record preview failure", "preview", p.ID, "error", err)
	}
	m.log.Warn("preview boot failed", "preview", p.ID, "error", cause)
	return cause
}

// makeRoom enforces MaxConcurrent, evicting idle previews until there is space
// for one more. It reports ErrAtCapacity when everything running is in use.
func (m *previewManager) makeRoom(ctx context.Context, exclude string) error {
	if m.policy.MaxConcurrent <= 0 {
		return nil
	}
	for {
		m.mu.Lock()
		count := 0
		for id := range m.live {
			if id != exclude {
				count++
			}
		}
		m.mu.Unlock()

		if count < m.policy.MaxConcurrent {
			return nil
		}
		evicted := m.evictIdle(ctx, exclude)
		if evicted == "" {
			return ErrAtCapacity
		}
		m.log.Info("evicted an idle preview to stay within max_concurrent", "evicted", evicted, "for", exclude)
	}
}

// evictIdle stops the least-recently-requested running preview, returning its
// ID, or "" when there is nothing to evict.
//
// "Least recently requested" rather than "oldest" because the question being
// answered is which preview somebody is least likely to be looking at right
// now. A preview booted an hour ago and refreshed a second ago is in use; one
// booted a minute ago and untouched since is not.
func (m *previewManager) evictIdle(ctx context.Context, exclude string) string {
	previews, err := m.store.List(ctx)
	if err != nil {
		m.log.Warn("could not list previews to evict", "error", err)
		return ""
	}
	byID := make(map[string]*preview.Preview, len(previews))
	for _, p := range previews {
		byID[p.ID] = p
	}

	m.mu.Lock()
	candidates := make([]string, 0, len(m.live))
	for id := range m.live {
		if id != exclude {
			candidates = append(candidates, id)
		}
	}
	m.mu.Unlock()

	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		li, lj := lastRequest(byID[candidates[i]]), lastRequest(byID[candidates[j]])
		if li.Equal(lj) {
			// Ties go to the lexically first ID so eviction is deterministic
			// rather than dependent on map iteration order.
			return candidates[i] < candidates[j]
		}
		return li.Before(lj)
	})

	victim := candidates[0]
	if err := m.Stop(ctx, victim, "evicted to make room for another preview"); err != nil {
		m.log.Warn("could not evict preview", "preview", victim, "error", err)
		return ""
	}
	return victim
}

// lastRequest is the ordering key for eviction, tolerant of a preview that has
// gone missing from the store between listing and sorting.
func lastRequest(p *preview.Preview) time.Time {
	if p == nil {
		return time.Time{}
	}
	if p.LastRequestAt.IsZero() {
		return p.CreatedAt
	}
	return p.LastRequestAt
}

// takeInstance hands a preview's in-memory half to exactly one caller. The
// reaper, a drain and an explicit `preview down` can all reach a stop at once;
// deleting under the lock is what decides which of them does the work, and the
// losers see nil.
func (m *previewManager) takeInstance(id string) *previewInstance {
	m.mu.Lock()
	instance := m.live[id]
	delete(m.live, id)
	gone := m.onInstanceGone
	m.mu.Unlock()
	if instance != nil && gone != nil {
		gone()
	}
	return instance
}

// onInstanceGoneCallback installs the notification takeInstance sends. It is
// set under the lock because the reaper is already running by the time ingress
// is built, and it is the reaper that most often takes an instance down.
func (m *previewManager) onInstanceGoneCallback(fn func()) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onInstanceGone = fn
}

// stopInstance tears down a preview's in-memory half without touching the store
// or its state. Returns whether there was anything to stop.
func (m *previewManager) stopInstance(id string) bool {
	instance := m.takeInstance(id)
	if instance == nil {
		return false
	}
	instance.close()
	return true
}

// Stop takes a preview's VM down and records it as stopped, keeping whatever
// state it declared. The record and its hostnames stay: the next request boots
// it again, onto the state this stop wrote out.
func (m *previewManager) Stop(ctx context.Context, id, reason string) error {
	return m.stop(ctx, id, reason, true)
}

// StopWithoutState takes a preview down and throws away what it was holding.
// It is what `preview down --no-state` and Remove use: both are somebody saying
// this preview's contents are finished with.
func (m *previewManager) StopWithoutState(ctx context.Context, id, reason string) error {
	return m.stop(ctx, id, reason, false)
}

func (m *previewManager) stop(ctx context.Context, id, reason string, keepState bool) error {
	if !m.enabled() {
		return ErrPreviewsDisabled
	}

	// The instance comes out of m.live before anything slow happens. A capture
	// takes as long as a tar of a database, and nothing may route into a VM
	// whose servers are being shut down underneath it.
	instance := m.takeInstance(id)

	p, err := m.store.Get(ctx, id)
	if errors.Is(err, preview.ErrNotFound) {
		if instance != nil {
			instance.close()
		}
		return nil
	}
	if err != nil {
		if instance != nil {
			instance.close()
		}
		return err
	}
	// Somebody else already took this preview down, or is still capturing it.
	// Overwriting their row with "stopped" while their tar is running would
	// hand the next request a VM that is halfway gone.
	if instance == nil && (p.State == preview.StateStopped || p.State == preview.StateStopping) {
		return nil
	}

	if instance != nil {
		m.captureState(ctx, p, instance, keepState)
		instance.close()
	}

	p.State = preview.StateStopped
	p.StartedAt = time.Time{}
	p.ExpiresAt = time.Time{}
	p.SessionID = ""
	p.UpdatedAt = m.now().UTC()
	if err := m.store.Put(ctx, p); err != nil {
		return err
	}

	m.log.Info("preview stopped", "preview", id, "reason", reason)
	return nil
}

// captureState writes a stopping preview's declared state out, updating the
// record in place with what happened. It never returns an error: a capture that
// fails must not keep a drain or a shutdown waiting, and must not leave a VM
// running because its data could not be saved. The reason lands on the record
// instead, where `preview ls` and `preview logs` can report it.
func (m *previewManager) captureState(ctx context.Context, p *preview.Preview, instance *previewInstance, keepState bool) {
	if !keepState || m.snapshots == nil || instance.cfg == nil {
		return
	}
	// A fork's branch is written by somebody without push access. Whatever their
	// code put on disk — including anything it wrote out of the project's own
	// secrets — is not going to sit on the operator's host for a month.
	if p.Fork {
		return
	}

	captureCtx, cancel := m.captureContext(ctx)
	defer cancel()

	st := instance.state()
	proxy := instance.sandbox.BareRunner()
	if proxy == nil {
		return
	}

	// A repository that declares nothing may still have written into the state
	// directory, so the guest is asked before the preview is moved into
	// "stopping" — a preview that holds nothing tears down in exactly the number
	// of calls it always did.
	if !st.Declared() {
		has, err := preview.HasState(captureCtx, proxy, st)
		if err != nil {
			m.log.Warn("could not check a preview for state", "preview", p.ID, "error", err)
			return
		}
		if !has {
			return
		}
	}

	p.State = preview.StateStopping
	p.StateError = ""
	p.UpdatedAt = m.now().UTC()
	if err := m.store.Put(ctx, p); err != nil {
		m.log.Warn("could not record a preview as saving its state", "preview", p.ID, "error", err)
	}

	logs := m.logBuffer(p.ID)
	if err := preview.StopServices(captureCtx, instance.sandbox.Processes(),
		instance.cfg, p.ID, preview.DefaultStopGrace); err != nil {
		// The servers could not be asked to stop, so whatever is captured next
		// was taken from under a running process. Worth saying, not worth
		// abandoning the capture over: a database that was not shut down cleanly
		// still restores more often than no database at all.
		m.log.Warn("could not stop a preview's services before capturing its state",
			"preview", p.ID, "error", err)
		logs.Append(fmt.Sprintf("==> could not stop services before saving state: %v\n", err))
	}

	meta, err := preview.Capture(captureCtx, preview.CaptureOpts{
		Proxy:          proxy,
		Runner:         instance.sandbox.GetRunner(),
		ShellSessionID: instance.sandbox.GetShellSessionID(),
		Store:          m.snapshots,
		ID:             instance.snapshotID,
		State:          st,
		MaxBytes:       m.stateCap(st),
		Meta: snapshot.Meta{
			Commit: instance.commit,
			Hosts:  p.Hosts(),
			Ref:    p.Ref,
		},
		OnStep: func(name string) {
			logs.Append(fmt.Sprintf("==> %s\n", name))
		},
		OnOutput: func(_ string, stdout, stderr string) {
			logs.Append(stdout)
			logs.Append(stderr)
		},
	})
	if err != nil {
		p.StateError = err.Error()
		m.log.Warn("could not save a preview's state", "preview", p.ID, "error", err)
		logs.Append(fmt.Sprintf("==> saving state failed: %v\n", err))
		return
	}
	if meta.CreatedAt.IsZero() {
		// Nothing was there after all; the declared paths do not exist in this
		// guest yet.
		return
	}

	p.StateSavedAt = meta.CreatedAt
	p.StateBytes = meta.Bytes
	p.StateError = ""
	m.log.Info("saved preview state", "preview", p.ID, "bytes", meta.Bytes)
}

// snapshotID resolves a preview's archive identity, reporting false when there
// is no way to work it out — a manager built without a resolver, or a project
// that has since been removed from projects.toml.
func (m *previewManager) snapshotID(ctx context.Context, p *preview.Preview) (snapshot.ID, bool) {
	if m.snapshotIDs == nil {
		return snapshot.ID{}, false
	}
	id, err := m.snapshotIDs(ctx, p)
	if err != nil {
		m.log.Warn("could not resolve where a preview's state is kept",
			"preview", p.ID, "error", err)
		return snapshot.ID{}, false
	}
	return id, true
}

// stateCap is the ceiling on one preview's archive: the repository's own
// max_size, brought down to the operator's if that is lower.
func (m *previewManager) stateCap(st projconfig.PreviewState) int64 {
	want := st.MaxSizeBytes()
	limit := m.policy.MaxStateBytes
	switch {
	case limit <= 0:
		return want
	case want <= 0 || want > limit:
		return limit
	default:
		return want
	}
}

// captureContext bounds one capture and detaches it from whoever asked for the
// stop. The caller's context is the wrong clock here: a drain carries an RPC
// deadline, and `kvarn local preview` reaches its teardown with a context that
// the interrupt already cancelled. Neither is a reason to lose the data.
func (m *previewManager) captureContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := m.policy.StateTimeout
	if budget <= 0 {
		budget = defaultPreviewStateTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

// Remove stops a preview and forgets it, releasing its hostnames and dropping
// whatever state it had stored.
//
// Nothing is captured on the way out: this path is somebody saying the preview
// is finished with, and keeping an archive for a preview that no longer exists
// would leave data on disk that nothing can ever restore or explain.
func (m *previewManager) Remove(ctx context.Context, id string) error {
	if !m.enabled() {
		return ErrPreviewsDisabled
	}
	if err := m.StopWithoutState(ctx, id, "removed"); err != nil {
		return err
	}
	m.ResetState(ctx, id)
	m.mu.Lock()
	delete(m.logs, id)
	m.mu.Unlock()
	return m.store.Delete(ctx, id)
}

// ResetState drops a preview's stored state, so the next boot comes up as if
// nothing had ever run there. It is best-effort on the archive and exact on the
// record: an archive that could not be deleted is logged, but the record must
// not keep claiming state that is no longer restorable.
func (m *previewManager) ResetState(ctx context.Context, id string) {
	if m.snapshots == nil {
		return
	}
	p, err := m.store.Get(ctx, id)
	if err != nil {
		return
	}
	sid, ok := m.snapshotID(ctx, p)
	if !ok {
		return
	}
	if err := m.snapshots.Delete(sid); err != nil {
		m.log.Warn("could not delete a preview's stored state", "preview", id, "error", err)
		return
	}
	if p.StateSavedAt.IsZero() && p.StateBytes == 0 && p.StateError == "" {
		return
	}
	p.StateSavedAt = time.Time{}
	p.StateBytes = 0
	p.StateError = ""
	p.UpdatedAt = m.now().UTC()
	if err := m.store.Put(ctx, p); err != nil {
		m.log.Warn("could not clear a preview's state record", "preview", id, "error", err)
	}
}

// DialGuest opens a connection into a running preview's VM. It reports false
// when the preview is not running here, which is what ingress turns into a
// holding page rather than a proxy error.
func (m *previewManager) DialGuest(ctx context.Context, id string, port uint16) (net.Conn, bool, error) {
	m.mu.Lock()
	instance := m.live[id]
	m.mu.Unlock()

	if instance == nil || instance.sandbox == nil {
		return nil, false, nil
	}
	conn, err := instance.sandbox.DialGuest(ctx, port)
	if err != nil {
		return nil, true, err
	}
	return conn, true, nil
}

// IsLive reports whether the manager is currently holding a VM for a preview.
// The store's state can lag it briefly during a boot, and ingress needs the
// authoritative answer before it tries to proxy.
func (m *previewManager) IsLive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[id]
	return ok
}

// Reconcile settles what a dead orchestrator left behind. Every non-stopped row
// refers to a VM that died with the previous process, so it is moved back to
// stopped and the next request boots it again.
//
// Unlike a job, there is nothing to fail here and nothing to resume: a preview
// is defined by its ref, and re-deriving it from the ref is exactly what a boot
// does. Reconciliation is therefore just telling the truth about what is
// running, which is nothing.
func (m *previewManager) Reconcile(ctx context.Context) error {
	if !m.enabled() {
		return nil
	}
	reset, err := m.store.ResetLive(ctx)
	if err != nil {
		return fmt.Errorf("reconcile previews: %w", err)
	}
	if len(reset) > 0 {
		m.log.Info("reset previews left running by a previous process",
			"count", len(reset), "previews", reset)
	}
	return nil
}

// SetDraining changes whether the manager will start previews, stopping every
// running one when it is turned on.
//
// A preview cannot migrate: it is a VM in this process's address space, and
// nothing outside can reach it. So a drain stops them rather than waiting them
// out — there is no in-flight work to preserve, and the next request on whatever
// host is serving boots the preview there.
func (m *previewManager) SetDraining(ctx context.Context, draining bool) {
	if !m.enabled() {
		return
	}
	m.mu.Lock()
	m.draining = draining
	ids := make([]string, 0, len(m.live))
	if draining {
		for id := range m.live {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()

	m.stopAll(ctx, ids, "host draining")
}

// stopAll takes several previews down at once.
//
// Sequentially would be simpler, and was enough while stopping a preview meant
// destroying a VM. A capture is a tar of a database, so N previews stopped one
// after another is N state timeouts end to end — and the VMs are independent, so
// there is nothing to be gained by making them wait for each other. The bound
// keeps a host with a dozen previews from thrashing its disk.
func (m *previewManager) stopAll(ctx context.Context, ids []string, reason string) {
	sort.Strings(ids)

	const concurrency = 4
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := m.Stop(ctx, id, reason); err != nil {
				m.log.Warn("could not stop preview", "preview", id, "reason", reason, "error", err)
			}
		}()
	}
	wg.Wait()
}

// StartReaper runs idle, max-lifetime and closed-pull-request reaping until ctx
// is cancelled.
func (m *previewManager) StartReaper(ctx context.Context) {
	if !m.enabled() {
		return
	}
	if m.policy.IdleTimeout > 0 || m.policy.MaxLifetime > 0 {
		go func() {
			t := time.NewTicker(previewReapInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					m.Reap(ctx)
				}
			}
		}()
	}

	// Stored state is swept on its own clock too. Retention is measured in
	// weeks, so this is the slowest of the three and does no work at all on a
	// host where no preview has ever kept anything.
	if m.snapshots != nil && m.policy.StateRetention > 0 {
		go func() {
			t := time.NewTicker(previewPruneInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					m.PruneState(ctx)
				}
			}
		}()
	}

	// Closed pull requests are swept on their own, far slower clock: it costs a
	// forge call per auto-started preview, and a pull request that merged a
	// minute ago is not urgent — idle reaping has already taken its VM down.
	if m.prState != nil {
		go func() {
			t := time.NewTicker(previewPRSweepInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					m.ReapClosedPullRequests(ctx)
				}
			}
		}()
	}
}

// Reap stops previews that have gone idle or outlived their cap. Exported to
// the package so specs can drive it from a fake clock instead of waiting on the
// ticker.
func (m *previewManager) Reap(ctx context.Context) {
	if !m.enabled() {
		return
	}

	m.mu.Lock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	sort.Strings(ids)

	now := m.now().UTC()
	for _, id := range ids {
		p, err := m.store.Get(ctx, id)
		if err != nil {
			continue
		}

		reason := ""
		switch {
		case m.policy.MaxLifetime > 0 && !p.ExpiresAt.IsZero() && !now.Before(p.ExpiresAt):
			reason = fmt.Sprintf("reached its maximum lifetime of %s", m.policy.MaxLifetime)
		case m.policy.IdleTimeout > 0 && now.Sub(lastRequest(p)) >= m.policy.IdleTimeout:
			reason = fmt.Sprintf("idle for %s", m.policy.IdleTimeout)
		}
		if reason == "" {
			continue
		}
		if err := m.Stop(ctx, id, reason); err != nil {
			m.log.Warn("could not reap preview", "preview", id, "error", err)
		}
	}
}

// Shutdown stops every running preview. Called from the service's shutdown so
// VMs do not outlive the process that owns their networks.
func (m *previewManager) Shutdown(ctx context.Context) {
	if !m.enabled() {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	m.stopAll(ctx, ids, "orchestrator shutting down")
}

// previewPruneInterval is how often stored preview state is swept for archives
// nobody has come back to. Retention is measured in weeks, so an hourly tick is
// already far more often than it needs to be.
const previewPruneInterval = time.Hour

// PruneState drops state archives untouched for longer than the configured
// retention. An expired archive is not an error: the next boot of that preview
// comes up fresh and says so.
//
// Previews running right now are excluded — their archives are about to be
// rewritten by the stop that takes them down, and sweeping one out from under a
// live preview would silently turn a stop into a discard.
func (m *previewManager) PruneState(context.Context) {
	if m.snapshots == nil || m.policy.StateRetention <= 0 {
		return
	}
	report, err := m.pruneState(m.policy.StateRetention)
	if err != nil {
		m.log.Warn("could not prune stored preview state", "error", err)
		return
	}
	if report.Removed > 0 {
		m.log.Info("pruned stored preview state",
			"archives", report.Removed, "bytes", report.BytesFreed,
			"retention", m.policy.StateRetention)
	}
}

// pruneState is the sweep itself, taking the horizon as an argument so an
// operator running it by hand can name a different one.
func (m *previewManager) pruneState(retention time.Duration) (snapshot.PruneReport, error) {
	if m.snapshots == nil {
		return snapshot.PruneReport{}, ErrPreviewsDisabled
	}

	m.mu.Lock()
	live := make(map[snapshot.ID]struct{}, len(m.live))
	for _, instance := range m.live {
		live[instance.snapshotID] = struct{}{}
	}
	m.mu.Unlock()

	return m.snapshots.Prune(retention, func(id snapshot.ID) bool {
		_, running := live[id]
		return running
	})
}
