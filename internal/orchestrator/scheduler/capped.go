package scheduler

import (
	"errors"
	"fmt"
)

// ErrExceedsLimit is returned by Acquire when a request's own footprint breaks
// a limit its tenant is held to, so no amount of waiting could admit it. Such a
// request is failed rather than queued.
var ErrExceedsLimit = errors.New("scheduler: request exceeds a tenant limit")

// Limits caps what one scope — a project, or an API key — may hold at once.
// A zero field is unlimited, so the zero Limits caps nothing.
//
// Jobs and capacity are separate caps because they bound different things: a
// job count bounds how much of the host's attention one tenant can occupy
// regardless of how small its jobs are, while a capacity cap bounds the host it
// can take with one large one. Neither derives from the other.
type Limits struct {
	MaxJobs int
	Max     Capacity
}

// IsZero reports whether the limits cap nothing.
func (l Limits) IsZero() bool { return l == Limits{} }

// admits reports whether a scope already holding cur can take on req.
func (l Limits) admits(cur Usage, req Request) bool {
	if l.MaxJobs > 0 && cur.Jobs+1 > l.MaxJobs {
		return false
	}
	if l.Max.CPUMillis > 0 && cur.CPUMillis+req.CPUMillis > l.Max.CPUMillis {
		return false
	}
	if l.Max.MemBytes > 0 && cur.MemBytes+req.MemBytes > l.Max.MemBytes {
		return false
	}
	if l.Max.DiskBytes > 0 && cur.DiskBytes+req.DiskBytes > l.Max.DiskBytes {
		return false
	}
	return true
}

// exceeded names the limit req breaks on its own, or "" if it breaks none.
// Used to tell "wait your turn" apart from "this can never run". MaxJobs is
// not checked: any positive job cap has room for one job in an idle scope, so
// it can only ever mean "wait".
func (l Limits) exceeded(req Request) string {
	switch {
	case l.Max.CPUMillis > 0 && req.CPUMillis > l.Max.CPUMillis:
		return "cpu"
	case l.Max.MemBytes > 0 && req.MemBytes > l.Max.MemBytes:
		return "memory"
	case l.Max.DiskBytes > 0 && req.DiskBytes > l.Max.DiskBytes:
		return "disk"
	}
	return ""
}

// Capped wraps a Policy so that a waiter whose project or API key is already at
// its limit is hidden from it, and delegates the choice among the rest.
//
// Hiding rather than blocking is the whole point: a capped waiter that stayed
// at the head of a FIFO queue would stop every other tenant too, turning a cap
// meant to contain one project into a host-wide stall. A hidden waiter cannot
// be starved either, since what makes it eligible again is its own tenant's
// jobs finishing, and it keeps its place in line among eligible waiters.
type Capped struct {
	Inner Policy
}

// Next implements Policy.
func (c Capped) Next(st State) int {
	inner := c.Inner
	if inner == nil {
		inner = FIFO{}
	}

	// Index i of the filtered queue is index visible[i] of the real one.
	eligible := make([]Waiting, 0, len(st.Queue))
	visible := make([]int, 0, len(st.Queue))
	for i, w := range st.Queue {
		if !admissible(st.Running, w.Request) {
			continue
		}
		eligible = append(eligible, w)
		visible = append(visible, i)
	}
	if len(eligible) == 0 {
		return -1
	}

	sub := st
	sub.Queue = eligible
	j := inner.Next(sub)
	if j < 0 || j >= len(visible) {
		return -1
	}
	return visible[j]
}

// Precheck implements the Policy precheck: a request whose own footprint breaks
// one of its limits is rejected at Acquire rather than queued forever.
func (c Capped) Precheck(req Request) error {
	if dim := req.ProjectLimits.exceeded(req); dim != "" && req.Tenant.Project != "" {
		return fmt.Errorf("%w: project %q %s limit", ErrExceedsLimit, req.Tenant.Project, dim)
	}
	if dim := req.KeyLimits.exceeded(req); dim != "" && req.Tenant.KeyID != "" {
		return fmt.Errorf("%w: API key %s limit", ErrExceedsLimit, dim)
	}
	return nil
}

// admissible reports whether req fits within both of its tenant's limits given
// what is running now.
//
// A scope with an empty identifier is never capped. Otherwise every job would
// share one counter whenever it cannot be attributed — with auth disabled all
// jobs carry an empty key — and a cap meant per key would silently become a
// host-wide one.
func admissible(running map[Tenant]Usage, req Request) bool {
	if p := req.Tenant.Project; p != "" && !req.ProjectLimits.IsZero() {
		cur := scopeUsage(running, func(t Tenant) bool { return t.Project == p })
		if !req.ProjectLimits.admits(cur, req) {
			return false
		}
	}
	if k := req.Tenant.KeyID; k != "" && !req.KeyLimits.IsZero() {
		cur := scopeUsage(running, func(t Tenant) bool { return t.KeyID == k })
		if !req.KeyLimits.admits(cur, req) {
			return false
		}
	}
	return true
}

// scopeUsage totals what every tenant matching the predicate holds. Running is
// keyed by the (project, key) pair, so one scope's total is a sum across the
// other axis — a project's jobs may have arrived on several keys.
func scopeUsage(running map[Tenant]Usage, match func(Tenant) bool) Usage {
	var out Usage
	for t, u := range running {
		if !match(t) {
			continue
		}
		out.Jobs += u.Jobs
		out.CPUMillis += u.CPUMillis
		out.MemBytes += u.MemBytes
		out.DiskBytes += u.DiskBytes
	}
	return out
}
