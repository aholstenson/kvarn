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
	"sync"
)

// ErrTooLarge is returned by Acquire when a request's footprint exceeds the
// scheduler's total capacity in any dimension. The request is never queued.
var ErrTooLarge = errors.New("scheduler: request exceeds total capacity")

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
}

// Scheduler admits Requests against a fixed Capacity pool, queueing those that
// don't fit. Strict FIFO: the head of the queue blocks every later waiter until
// it can be admitted, even if a later waiter would fit in the current free
// capacity. Replace tryAdmitLocked to swap in a smarter policy.
type Scheduler struct {
	mu        sync.Mutex
	total     Capacity
	used      Capacity
	queue     []*waiter
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
	req     Request
	done    chan struct{}
	granted bool
}

// New constructs a bounded scheduler. The Total in opts has CPU and disk
// overcommit already applied by the caller.
func New(opts Options) *Scheduler {
	return &Scheduler{
		total:     opts.Total,
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

	s.mu.Lock()
	if req.CPUMillis > s.total.CPUMillis ||
		req.MemBytes > s.total.MemBytes ||
		req.DiskBytes > s.total.DiskBytes {
		s.mu.Unlock()
		return nil, ErrTooLarge
	}

	// Fast path: no one waiting and the request fits right now.
	if len(s.queue) == 0 && s.diskGateOpenLocked() && s.freeLocked().fits(req) {
		s.used.CPUMillis += req.CPUMillis
		s.used.MemBytes += req.MemBytes
		s.used.DiskBytes += req.DiskBytes
		s.mu.Unlock()
		return s.newLease(req), nil
	}

	w := &waiter{req: req, done: make(chan struct{})}
	s.queue = append(s.queue, w)
	notes := s.collectNotificationsLocked()
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
			s.deductLocked(req)
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

func (s *Scheduler) freeLocked() Capacity {
	return Capacity{
		CPUMillis: s.total.CPUMillis - s.used.CPUMillis,
		MemBytes:  s.total.MemBytes - s.used.MemBytes,
		DiskBytes: s.total.DiskBytes - s.used.DiskBytes,
	}
}

func (s *Scheduler) deductLocked(req Request) {
	s.used.CPUMillis -= req.CPUMillis
	s.used.MemBytes -= req.MemBytes
	s.used.DiskBytes -= req.DiskBytes
}

func (s *Scheduler) release(granted Capacity) {
	s.mu.Lock()
	s.used.CPUMillis -= granted.CPUMillis
	s.used.MemBytes -= granted.MemBytes
	s.used.DiskBytes -= granted.DiskBytes
	notes := s.tryAdmitLocked()
	s.mu.Unlock()
	fireNotifications(notes)
}

// tryAdmitLocked drains the head of the queue while it fits, then collects
// position-change notifications for the remaining waiters. Strict FIFO is
// enforced here: if the head doesn't fit we stop, even if later waiters would.
func (s *Scheduler) tryAdmitLocked() []notification {
	if !s.diskGateOpenLocked() {
		return nil
	}
	admitted := 0
	for len(s.queue) > 0 {
		head := s.queue[0]
		if !s.freeLocked().fits(head.req) {
			break
		}
		s.used.CPUMillis += head.req.CPUMillis
		s.used.MemBytes += head.req.MemBytes
		s.used.DiskBytes += head.req.DiskBytes
		head.granted = true
		s.queue = s.queue[1:]
		close(head.done)
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
		s: s,
		granted: Capacity{
			CPUMillis: req.CPUMillis,
			MemBytes:  req.MemBytes,
			DiskBytes: req.DiskBytes,
		},
	}
}
