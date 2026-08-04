package scheduler

import (
	"sort"
	"time"
)

// Fair reorders the queue by who deserves the host next, then hands the
// reordered view to Inner to make the actual choice. It ranks by, in order:
//
//  1. Effective priority, highest first — what the operator asked for.
//  2. Dominant resource share, lowest first — a project running nothing is
//     served before one already holding a large slice of the host.
//  3. Arrival, oldest first, which is what the sort's stability preserves.
//
// Dominant share is the multi-resource generalization of "who is using more":
// the largest fraction of any single dimension a project holds. Comparing
// vCPUs against gigabytes is meaningless, but comparing "35% of the host's
// memory" against "20% of its CPU" is not, so the largest such fraction is
// what stands for a project's claim on the host. It costs nothing to
// configure, and on a single-project host every waiter ties and the order
// falls back to arrival.
//
// Fairness is measured per project rather than per (project, key): the
// question it answers is which body of work is hogging the host, and one
// project's jobs are that whether they arrived on one key or several.
type Fair struct {
	// Inner picks from the reordered queue. Nil means FIFO, which on a
	// reordered queue means "the most deserving waiter that fits".
	Inner Policy
	// AgeStep is how much waiting it takes to gain one level of effective
	// priority, so a low-priority job is not starved by a stream of
	// high-priority ones. Zero disables aging, letting priority strictly
	// dominate.
	AgeStep time.Duration
}

// Next implements Policy.
func (f Fair) Next(st State) int {
	inner := f.Inner
	if inner == nil {
		inner = FIFO{}
	}
	if len(st.Queue) < 2 {
		return inner.Next(st)
	}

	order := f.rank(st)

	reordered := make([]Waiting, len(order))
	for i, idx := range order {
		reordered[i] = st.Queue[idx]
	}
	sub := st
	sub.Queue = reordered

	j := inner.Next(sub)
	if j < 0 || j >= len(order) {
		return -1
	}
	return order[j]
}

// rank returns queue indices in preference order.
func (f Fair) rank(st State) []int {
	type entry struct {
		idx      int
		priority int
		share    float64
	}

	// Aging is bounded by the highest priority actually queued, so it can only
	// close a gap the operator created and never open one. Without the clamp,
	// a queue where nobody set a priority would order purely by age bucket and
	// dominant share would stop mattering at all.
	ceiling := st.Queue[0].Priority
	for _, w := range st.Queue[1:] {
		if w.Priority > ceiling {
			ceiling = w.Priority
		}
	}

	shares := make(map[string]float64, 4)
	entries := make([]entry, len(st.Queue))
	for i, w := range st.Queue {
		p := w.Priority
		if f.AgeStep > 0 && p < ceiling {
			p += int(st.Now.Sub(w.EnqueuedAt) / f.AgeStep)
			if p > ceiling {
				p = ceiling
			}
		}
		proj := w.Tenant.Project
		share, ok := shares[proj]
		if !ok {
			share = dominantShare(st.Running, st.Total, proj)
			shares[proj] = share
		}
		entries[i] = entry{idx: i, priority: p, share: share}
	}

	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].priority != entries[b].priority {
			return entries[a].priority > entries[b].priority
		}
		return entries[a].share < entries[b].share
	})

	out := make([]int, len(entries))
	for i, e := range entries {
		out[i] = e.idx
	}
	return out
}

// dominantShare is the largest fraction of any one dimension of the pool that
// the named project currently holds.
func dominantShare(running map[Tenant]Usage, total Capacity, project string) float64 {
	u := scopeUsage(running, func(t Tenant) bool { return t.Project == project })
	share := ratio(u.CPUMillis, total.CPUMillis)
	if r := ratio(u.MemBytes, total.MemBytes); r > share {
		share = r
	}
	if r := ratio(u.DiskBytes, total.DiskBytes); r > share {
		share = r
	}
	return share
}

// ratio guards the zero total a Capacity permits but a configured pool never
// has, so an unusable dimension reads as "claims nothing" rather than NaN.
func ratio(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total)
}
