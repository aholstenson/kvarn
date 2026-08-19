// Package scheduler caps concurrent jobs by a (cpu, memory, disk) capacity pool
// and queues requests that don't fit. Memory is strict; CPU and disk are
// overcommittable (multipliers pre-applied to Total).
//
// Disk is overcommitted because a job is charged the *virtual* size of its VM
// disk while the image itself is thin — a qcow2 overlay on Linux, a sparse raw
// file on macOS — so a 16 GiB request typically costs a fraction of that on the
// host. Charging the full request would idle most of the pool. Prediction is
// not possible here (a job's real growth depends on what it builds), so the
// overcommit is backed by measurement instead: WatchHostDisk feeds real free
// space in, and admission stops entirely while it is below DiskFloorBytes.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrTooLarge is returned by Acquire when a request's footprint exceeds the
// scheduler's total capacity in any dimension. The request is never queued.
var ErrTooLarge = errors.New("scheduler: request exceeds total capacity")

// ErrQueueFull is returned by Acquire when the queue is at MaxQueue and the
// request cannot be admitted right away. It is backpressure, not a failure of
// the request: the same request submitted against a shorter queue would be
// accepted.
var ErrQueueFull = errors.New("scheduler: admission queue is full")

// Options configures a new Scheduler. Total has CPU and disk overcommit already
// applied; the multipliers are retained for reporting.
type Options struct {
	Total          Capacity
	CPUOvercommit  float64
	DiskOvercommit float64
	// DiskPath is the filesystem WatchHostDisk measures — the one VM disks are
	// allocated on. Empty disables the guard.
	DiskPath string
	// DiskFloorBytes is how much real free space that filesystem must keep.
	// While measured free space is below it no request is admitted, whatever
	// the accounting pool says. Zero disables the guard.
	DiskFloorBytes uint64
	// MaxQueue bounds how many requests may wait at once. Zero is unbounded.
	// A queue is not free to hold: every waiter is a goroutine and, in the
	// orchestrator, a clone already on disk.
	MaxQueue int
	// Policy chooses which queued request is admitted next. Nil means FIFO.
	Policy Policy
	// Now overrides the clock used to stamp and compare queue wait times.
	// Nil means time.Now.
	Now func() time.Time
}

// Scheduler admits Requests against a fixed Capacity pool, queueing those that
// don't fit. It owns the mechanics — blocking, cancellation, leases, the host
// disk guard — while the admission order is the Policy's, defaulting to FIFO.
type Scheduler struct {
	mu        sync.Mutex
	total     Capacity
	used      Capacity
	running   map[Tenant]Usage
	queue     []*waiter
	maxQueue  int
	policy    Policy
	now       func() time.Time
	cpuOC     float64
	diskOC    float64
	unbounded bool

	// Host disk guard. diskAvail is only meaningful once the watchdog has
	// reported; until then the guard is open, so a slow first sample delays
	// no one.
	diskPath     string
	diskFloor    uint64
	diskAvail    uint64
	diskMeasured bool
}

type waiter struct {
	req        Request
	done       chan struct{}
	enqueuedAt time.Time
	granted    bool
}

// New constructs a bounded scheduler. The Total in opts has CPU and disk
// overcommit already applied by the caller.
func New(opts Options) *Scheduler {
	p := opts.Policy
	if p == nil {
		p = FIFO{}
	}
	clock := opts.Now
	if clock == nil {
		clock = time.Now
	}
	return &Scheduler{
		total:     opts.Total,
		running:   make(map[Tenant]Usage),
		maxQueue:  opts.MaxQueue,
		policy:    p,
		now:       clock,
		cpuOC:     opts.CPUOvercommit,
		diskOC:    opts.DiskOvercommit,
		diskPath:  opts.DiskPath,
		diskFloor: opts.DiskFloorBytes,
	}
}

// NewUnbounded returns a scheduler that admits every request immediately and
// never queues. It is the default when the orchestrator is configured without
// scheduler limits, so existing call sites and tests keep working unchanged.
func NewUnbounded() *Scheduler {
	return &Scheduler{unbounded: true}
}

// Acquire reserves the request's footprint, blocking until either the request
// can be admitted (returning a Lease) or ctx is cancelled. Requests larger than
// the total pool return ErrTooLarge without queueing.
func (s *Scheduler) Acquire(ctx context.Context, req Request) (Lease, error) {
	if s.unbounded {
		return &noopLease{granted: Capacity{
			CPUMillis: req.CPUMillis,
			MemBytes:  req.MemBytes,
			DiskBytes: req.DiskBytes,
		}}, nil
	}

	// Both checks below reject rather than queue, for the same reason: a
	// request they catch could not be admitted by an idle host, so waiting
	// would only postpone the failure until the caller gave up.
	if p, ok := s.policy.(Precheck); ok {
		if err := p.Precheck(req); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	if req.CPUMillis > s.total.CPUMillis ||
		req.MemBytes > s.total.MemBytes ||
		req.DiskBytes > s.total.DiskBytes {
		s.mu.Unlock()
		return nil, ErrTooLarge
	}

	// Every request joins the queue, even one that fits an idle pool. A fast
	// path around the policy would be a fast path around whatever the policy
	// enforces — a tenant's concurrency cap, a reservation held for the head —
	// and would apply exactly when the pool is empty enough for those to
	// matter. The extra allocation is nothing next to booting a VM.
	w := &waiter{req: req, done: make(chan struct{}), enqueuedAt: s.now()}
	s.queue = append(s.queue, w)
	notes := s.tryAdmitLocked()
	if w.granted {
		s.mu.Unlock()
		fireNotifications(notes)
		return s.newLease(req), nil
	}
	// The cap is checked only once the request is known to need a place in
	// line. Checking before the admission attempt would turn away a request
	// the pool could have run immediately — backfill admits past a full queue
	// whenever a small job fits — which is the opposite of what a queue bound
	// is for.
	// Our waiter is still the last entry: it was appended above, and any
	// admission since then removed only entries ahead of it.
	if s.maxQueue > 0 && len(s.queue) > s.maxQueue {
		s.queue = s.queue[:len(s.queue)-1]
		s.mu.Unlock()
		fireNotifications(notes)
		return nil, ErrQueueFull
	}
	if notes == nil {
		// Nothing moved, so tell the new arrival where it landed.
		notes = s.collectNotificationsLocked()
	}
	s.mu.Unlock()

	fireNotifications(notes)

	select {
	case <-w.done:
		return s.newLease(req), nil
	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			// Admitted between the channel close and our select picking ctx;
			// give the capacity back so it isn't leaked.
			s.creditLocked(req.Tenant, req.capacity())
			notes := s.tryAdmitLocked()
			s.mu.Unlock()
			fireNotifications(notes)
			return nil, ctx.Err()
		}
		for i, x := range s.queue {
			if x == w {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				break
			}
		}
		notes := s.tryAdmitLocked()
		s.mu.Unlock()
		fireNotifications(notes)
		return nil, ctx.Err()
	}
}

// ErrWouldBlock is returned by TryAcquire when the request cannot be admitted
// immediately. It is not a failure of the request: the same request will be
// admitted once the pool frees up.
var ErrWouldBlock = errors.New("scheduler: request cannot be admitted immediately")

// TryAcquire admits the request only if it fits right now, returning
// ErrWouldBlock instead of waiting.
//
// Acquire queues everything by design, which is right for a job: nobody is
// holding a connection open while it waits. A preview boot is the other case —
// an HTTP request is waiting on the answer — and there queueing behind an hour
// of jobs is worse than saying "at capacity" and letting the caller evict
// something or show a holding page.
//
// The request still goes through the policy, so a tenant cap or a reservation
// held for the head of the queue applies here exactly as it does to Acquire;
// what changes is only what happens when the answer is no.
func (s *Scheduler) TryAcquire(req Request) (Lease, error) {
	if s.unbounded {
		return &noopLease{granted: Capacity{
			CPUMillis: req.CPUMillis,
			MemBytes:  req.MemBytes,
			DiskBytes: req.DiskBytes,
		}}, nil
	}

	if p, ok := s.policy.(Precheck); ok {
		if err := p.Precheck(req); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	if req.CPUMillis > s.total.CPUMillis ||
		req.MemBytes > s.total.MemBytes ||
		req.DiskBytes > s.total.DiskBytes {
		s.mu.Unlock()
		return nil, ErrTooLarge
	}

	// Join the queue for one admission pass and leave again if it does not
	// carry us. Going through the queue rather than checking free capacity
	// directly is what keeps the policy authoritative: it may well decline a
	// request that fits, because something ahead of it has a claim.
	w := &waiter{req: req, done: make(chan struct{}), enqueuedAt: s.now()}
	s.queue = append(s.queue, w)
	notes := s.tryAdmitLocked()
	if w.granted {
		s.mu.Unlock()
		fireNotifications(notes)
		return s.newLease(req), nil
	}
	// Our waiter is still the last entry: it was appended above, and any
	// admission since then removed only entries ahead of it.
	s.queue = s.queue[:len(s.queue)-1]
	s.mu.Unlock()

	fireNotifications(notes)
	return nil, ErrWouldBlock
}

// Snapshot returns a point-in-time view of pool usage. Useful for /debug
// surfaces and tests.
func (s *Scheduler) Snapshot() (used, free Capacity, queueLen int) {
	if s.unbounded {
		return Capacity{}, Capacity{}, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used, s.freeLocked(), len(s.queue)
}

// QueueFull reports whether the queue is at its bound. It is a hint for
// callers that can refuse work before doing expensive setup — the authoritative
// answer is ErrQueueFull from Acquire, since the queue moves in between.
func (s *Scheduler) QueueFull() bool {
	if s.unbounded || s.maxQueue <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) >= s.maxQueue
}

// TenantUsage returns a copy of what each tenant currently holds. Only tenants
// with a running job appear.
func (s *Scheduler) TenantUsage() map[Tenant]Usage {
	if s.unbounded {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Tenant]Usage, len(s.running))
	for t, u := range s.running {
		out[t] = u
	}
	return out
}

func (s *Scheduler) freeLocked() Capacity {
	return Capacity{
		CPUMillis: s.total.CPUMillis - s.used.CPUMillis,
		MemBytes:  s.total.MemBytes - s.used.MemBytes,
		DiskBytes: s.total.DiskBytes - s.used.DiskBytes,
	}
}

// chargeLocked books capacity against the pool and its tenant.
func (s *Scheduler) chargeLocked(t Tenant, c Capacity) {
	s.used.CPUMillis += c.CPUMillis
	s.used.MemBytes += c.MemBytes
	s.used.DiskBytes += c.DiskBytes

	u := s.running[t]
	u.CPUMillis += c.CPUMillis
	u.MemBytes += c.MemBytes
	u.DiskBytes += c.DiskBytes
	u.Jobs++
	s.running[t] = u
}

// creditLocked reverses chargeLocked. A tenant that drops to zero is deleted
// rather than left at zero, so the map tracks who is running now instead of
// growing once per project the host has ever seen.
func (s *Scheduler) creditLocked(t Tenant, c Capacity) {
	s.used.CPUMillis -= c.CPUMillis
	s.used.MemBytes -= c.MemBytes
	s.used.DiskBytes -= c.DiskBytes

	u := s.running[t]
	u.CPUMillis -= c.CPUMillis
	u.MemBytes -= c.MemBytes
	u.DiskBytes -= c.DiskBytes
	u.Jobs--
	if u.Jobs <= 0 {
		delete(s.running, t)
		return
	}
	s.running[t] = u
}

func (s *Scheduler) release(t Tenant, granted Capacity) {
	s.mu.Lock()
	s.creditLocked(t, granted)
	notes := s.tryAdmitLocked()
	s.mu.Unlock()
	fireNotifications(notes)
}

// tryAdmitLocked asks the policy for waiters to admit until it declines, then
// collects position-change notifications for whoever is left. The policy sees a
// State refreshed after each admission, so it decides against the capacity that
// actually remains rather than the capacity it started with.
func (s *Scheduler) tryAdmitLocked() []notification {
	if !s.diskGateOpenLocked() {
		return nil
	}

	// One Waiting slice for the whole drain, kept in step with s.queue as
	// entries leave it.
	view := make([]Waiting, len(s.queue))
	for i, w := range s.queue {
		view[i] = Waiting{Request: w.req, EnqueuedAt: w.enqueuedAt}
	}

	now := s.now()
	admitted := 0
	for len(s.queue) > 0 {
		i := s.policy.Next(State{
			Free:    s.freeLocked(),
			Total:   s.total,
			Queue:   view,
			Running: s.running,
			Now:     now,
		})
		if i < 0 || i >= len(s.queue) {
			break
		}
		// A policy is pluggable, so its answer is checked rather than trusted:
		// admitting a request that does not fit would drive used past total and
		// underflow the unsigned free capacity on release.
		w := s.queue[i]
		if !s.freeLocked().Fits(w.req) {
			slog.Error("admission policy chose a request that does not fit; ignoring",
				"index", i, "cpu_millis", w.req.CPUMillis, "mem_bytes", w.req.MemBytes, "disk_bytes", w.req.DiskBytes)
			break
		}

		s.chargeLocked(w.req.Tenant, w.req.capacity())
		w.granted = true
		s.queue = append(s.queue[:i], s.queue[i+1:]...)
		view = append(view[:i], view[i+1:]...)
		close(w.done)
		admitted++
	}
	if admitted == 0 {
		return nil
	}
	return s.collectNotificationsLocked()
}

// notification is a deferred OnWait call captured under the lock so the actual
// invocation can happen outside it.
type notification struct {
	fn  func(WaitEvent)
	evt WaitEvent
}

func (s *Scheduler) collectNotificationsLocked() []notification {
	if len(s.queue) == 0 {
		return nil
	}
	free := s.freeLocked()
	diskLow := !s.diskGateOpenLocked()
	out := make([]notification, 0, len(s.queue))
	for i, w := range s.queue {
		if w.req.OnWait == nil {
			continue
		}
		out = append(out, notification{
			fn: w.req.OnWait,
			evt: WaitEvent{
				Position:    i + 1,
				Need:        w.req,
				Free:        free,
				HostDiskLow: diskLow,
			},
		})
	}
	return out
}

func fireNotifications(notes []notification) {
	for _, n := range notes {
		n.fn(n.evt)
	}
}

func (s *Scheduler) newLease(req Request) Lease {
	return &realLease{
		s:       s,
		tenant:  req.Tenant,
		granted: req.capacity(),
	}
}
