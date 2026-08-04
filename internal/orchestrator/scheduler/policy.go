package scheduler

import "time"

// Tenant identifies who a request is on behalf of. Both axes are recorded
// because they answer different questions: a project is the unit of work an
// operator reasons about, while an API key is the unit of trust that submitted
// it, and one key drives many projects.
//
// The zero Tenant is a valid key in Scheduler.TenantUsage — requests that name
// no tenant are accounted together rather than being dropped, so per-tenant
// totals always sum to the pool's used capacity.
type Tenant struct {
	Project string
	KeyID   string
}

// Usage is what one tenant currently holds. Jobs is tracked alongside the
// capacity because a concurrency cap and a capacity cap answer different
// questions and neither derives from the other.
type Usage struct {
	Capacity
	Jobs int
}

// Waiting is one queued request as a Policy sees it.
type Waiting struct {
	Request
	// EnqueuedAt is when the request joined the queue, stamped by the
	// scheduler rather than the caller so a policy can trust it as a
	// measure of how long the request has actually been waiting.
	EnqueuedAt time.Time
}

// State is the scheduler's view of itself at the moment a Policy is asked to
// choose. Running maps every tenant holding capacity to what it holds; it is
// owned by the scheduler and must not be modified or retained.
type State struct {
	Free    Capacity
	Total   Capacity
	Queue   []Waiting
	Running map[Tenant]Usage
}

// Policy decides which queued request is admitted next. Separating that
// decision from Acquire's blocking, cancellation and lease bookkeeping is what
// lets an admission order be written and tested as a pure function.
//
// Next returns an index into st.Queue, or a negative number to admit nothing
// for now. The scheduler calls it repeatedly, with a State refreshed after each
// admission, until it declines — so a policy expresses "admit these three" by
// answering three times rather than by returning a set.
//
// Implementations run inside the scheduler's critical section. They must not
// block, call back into the Scheduler, or retain State beyond the call. An
// index whose request does not fit the free capacity is ignored, so a buggy
// policy cannot drive the pool negative.
type Policy interface {
	Next(st State) int
}

// FIFO admits the oldest waiter as soon as it fits and nothing else until then.
// The head of the queue therefore blocks every later request even when a later
// one would fit in the capacity available right now.
//
// That head-of-line blocking is the cost of the guarantee: no request can be
// overtaken, so none can be starved by a stream of smaller ones. It is the
// default because it is the policy that needs no configuration to be fair.
type FIFO struct{}

// Next implements Policy.
func (FIFO) Next(st State) int {
	if len(st.Queue) == 0 || !st.Free.Fits(st.Queue[0].Request) {
		return -1
	}
	return 0
}
