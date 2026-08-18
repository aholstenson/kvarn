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
	// Close stops the serve processes and destroys the VM.
	Close()
}

// previewBootSandbox is the wider surface the boot itself drives. It is a
// superset of PreviewSandbox: once the boot returns, only the two methods above
// are still needed, and narrowing the type the manager holds keeps the manager
// testable against something far smaller than a sandbox session.
type previewBootSandbox interface {
	PreviewSandbox

	CanDialGuest() bool
	Processes() sandbox.ProcessRunner
	GetRunner() sandbox.RunnerProxy
	GetShellSessionID() string
	GetWorkingDir() string
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

// previewBoot is the result of provisioning one preview.
type previewBoot struct {
	Sandbox   PreviewSandbox
	Sites     []preview.Site
	SessionID string
	Lease     scheduler.Lease
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

	mu sync.Mutex
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

// Register creates or refreshes the durable record for a preview of a ref,
// without booting it. It is what `kvarn preview up` calls first: the row (and
// with it the hostname) has to exist before anything can route to the preview.
func (m *previewManager) Register(ctx context.Context, project, ref string) (*preview.Preview, error) {
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
		return existing, nil
	}

	p := &preview.Preview{
		ID:        id,
		Project:   project,
		Ref:       ref,
		State:     preview.StateStopped,
		CreatedAt: now,
		UpdatedAt: now,
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
func (m *previewManager) Ensure(ctx context.Context, id string) (*preview.Preview, error) {
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
		sandbox:   boot.Sandbox,
		sites:     boot.Sites,
		sessionID: boot.SessionID,
		lease:     boot.Lease,
		startedAt: m.now().UTC(),
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

// stopInstance tears down a preview's in-memory half without touching the
// store. Returns whether there was anything to stop.
func (m *previewManager) stopInstance(id string) bool {
	m.mu.Lock()
	instance := m.live[id]
	delete(m.live, id)
	m.mu.Unlock()

	if instance == nil {
		return false
	}
	instance.close()
	return true
}

// Stop takes a preview's VM down and records it as stopped. The record and its
// hostnames stay: the next request boots it again.
func (m *previewManager) Stop(ctx context.Context, id, reason string) error {
	if !m.enabled() {
		return ErrPreviewsDisabled
	}

	stopped := m.stopInstance(id)

	p, err := m.store.Get(ctx, id)
	if errors.Is(err, preview.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.State == preview.StateStopped && !stopped {
		return nil
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

// Remove stops a preview and forgets it, releasing its hostnames.
func (m *previewManager) Remove(ctx context.Context, id string) error {
	if !m.enabled() {
		return ErrPreviewsDisabled
	}
	if err := m.Stop(ctx, id, "removed"); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.logs, id)
	m.mu.Unlock()
	return m.store.Delete(ctx, id)
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

	sort.Strings(ids)
	for _, id := range ids {
		if err := m.Stop(ctx, id, "host draining"); err != nil {
			m.log.Warn("could not stop preview while draining", "preview", id, "error", err)
		}
	}
}

// StartReaper runs idle and max-lifetime reaping until ctx is cancelled.
func (m *previewManager) StartReaper(ctx context.Context) {
	if !m.enabled() {
		return
	}
	if m.policy.IdleTimeout <= 0 && m.policy.MaxLifetime <= 0 {
		return
	}
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
	sort.Strings(ids)

	for _, id := range ids {
		if err := m.Stop(ctx, id, "orchestrator shutting down"); err != nil {
			m.log.Warn("could not stop preview during shutdown", "preview", id, "error", err)
		}
	}
}
