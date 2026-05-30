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

// fits reports whether req can be admitted against free.
func (c Capacity) fits(req Request) bool {
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
	OnWait    func(WaitEvent)
}

// WaitEvent is delivered to Request.OnWait when a waiter enqueues and when its
// position shifts as earlier waiters are admitted or cancelled.
type WaitEvent struct {
	Position int
	Need     Request
	Free     Capacity
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

func (l *noopLease) Release()           {}
func (l *noopLease) Granted() Capacity  { return l.granted }

// realLease is the bounded scheduler's lease. release is invoked at most once
// thanks to sync.Once, so accidental double-release never double-credits.
type realLease struct {
	s       *Scheduler
	granted Capacity
	once    sync.Once
}

func (l *realLease) Release() {
	l.once.Do(func() {
		l.s.release(l.granted)
	})
}

func (l *realLease) Granted() Capacity { return l.granted }
