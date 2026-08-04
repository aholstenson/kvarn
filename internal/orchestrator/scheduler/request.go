package scheduler

import "sync"

// Capacity is a three-dimensional resource bundle: CPU is measured in millicpus
// (1 vCPU = 1000), memory and disk in bytes. The Total Capacity passed to New
// already has CPU overcommit applied; consumers see the post-overcommit pool.
type Capacity struct {
	CPUMillis uint64
	MemBytes  uint64
	DiskBytes uint64
}

// Fits reports whether req can be admitted against the capacity in c.
func (c Capacity) Fits(req Request) bool {
	return req.CPUMillis <= c.CPUMillis &&
		req.MemBytes <= c.MemBytes &&
		req.DiskBytes <= c.DiskBytes
}

// Request is a single admission request. OnWait, if non-nil, fires once on
// enqueue and again each time the waiter's queue position changes; it is never
// invoked when the request admits immediately.
type Request struct {
	CPUMillis uint64
	MemBytes  uint64
	DiskBytes uint64
	// Tenant attributes the request's capacity to whoever asked for it, so a
	// Policy can weigh what one project or key already holds. Optional: an
	// unset Tenant is accounted under the zero value.
	Tenant Tenant
	// ProjectLimits and KeyLimits cap what the request's project and API key
	// may hold at once. They travel with the request so a Policy can enforce
	// them without reading config from inside the scheduler's lock; the
	// consequence is that they are the limits in force when the job was
	// submitted, so raising a cap applies to jobs submitted after the change,
	// not to ones already queued.
	ProjectLimits Limits
	KeyLimits     Limits
	OnWait        func(WaitEvent)
}

// capacity is the footprint the request occupies once admitted.
func (r Request) capacity() Capacity {
	return Capacity{
		CPUMillis: r.CPUMillis,
		MemBytes:  r.MemBytes,
		DiskBytes: r.DiskBytes,
	}
}

// WaitEvent is delivered to Request.OnWait when a waiter enqueues and when its
// position shifts as earlier waiters are admitted or cancelled.
type WaitEvent struct {
	Position int
	Need     Request
	Free     Capacity
	// HostDiskLow reports that the host disk guard has admission paused. It
	// distinguishes the two reasons a job sits in the queue, which look
	// identical from the outside but need different operator action: Free
	// genuinely too small, versus Free looking ample while the filesystem
	// underneath the pool is nearly full.
	HostDiskLow bool
}

// Lease is the handle returned by Acquire. Release credits the scheduler with
// the granted capacity and is safe to call multiple times.
type Lease interface {
	Release()
	Granted() Capacity
}

// noopLease is the unbounded scheduler's lease — Release is a no-op and Granted
// echoes the requested capacity.
type noopLease struct {
	granted Capacity
}

func (l *noopLease) Release()          {}
func (l *noopLease) Granted() Capacity { return l.granted }

// realLease is the bounded scheduler's lease. release is invoked at most once
// thanks to sync.Once, so accidental double-release never double-credits. The
// tenant is carried along so the release credits the same per-tenant total the
// admission charged.
type realLease struct {
	s       *Scheduler
	tenant  Tenant
	granted Capacity
	once    sync.Once
}

func (l *realLease) Release() {
	l.once.Do(func() {
		l.s.release(l.tenant, l.granted)
	})
}

func (l *realLease) Granted() Capacity { return l.granted }
