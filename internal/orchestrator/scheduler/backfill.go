package scheduler

import "time"

// Backfill admits the oldest waiter that fits, skipping past ones that do not —
// so a queue of small jobs is not held up by a large job at its head — but only
// while those large jobs are still young. Once a waiter has been queued for
// Grace it holds the line: nothing behind it is admitted until it is.
//
// The usual way to backfill safely is to reserve a start time for the head and
// admit only jobs that provably finish before it. That needs a runtime estimate
// per job, which does not exist here — how long an agent runs depends on what
// it finds — so this trades the exact guarantee for a bounded one:
//
//   - While a waiter is younger than Grace, backfill is unrestricted and the
//     pool stays as full as the queue can make it.
//   - Once it reaches Grace, no *new* job is admitted ahead of it, so from then
//     on it is waiting only on jobs that were already running to finish.
//
// A large job is therefore delayed by at most Grace plus the residual runtime
// of the jobs let in during that window — never indefinitely, however long the
// stream of small jobs behind it. Grace is the whole trade-off: 0 is strict
// FIFO, and raising it buys utilization with the head's worst-case wait.
//
// Aging is per waiter, not just the head, so a run of large jobs cannot starve
// the second one in line while the first is admitted and re-queued behind it.
type Backfill struct {
	// Grace is how long a waiter may be skipped before it holds the line.
	// Zero makes every waiter hold the line immediately, which is FIFO.
	Grace time.Duration
}

// Next implements Policy.
func (b Backfill) Next(st State) int {
	for i, w := range st.Queue {
		if st.Free.Fits(w.Request) {
			return i
		}
		if st.Now.Sub(w.EnqueuedAt) >= b.Grace {
			// This waiter has been passed over long enough. Capacity freed
			// from here on is its own, so no one behind it may take it.
			return -1
		}
	}
	return -1
}
