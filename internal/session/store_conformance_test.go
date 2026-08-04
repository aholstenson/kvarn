package session_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/session"
	sqlitestore "github.com/aholstenson/kvarn/internal/session/sqlite"
)

// makeSession builds a Session with deterministic timestamps for store tests.
func makeSession(id, project string, state session.State, createdAt time.Time) *session.Session {
	return &session.Session{
		ID:          id,
		ProjectName: project,
		Prompt:      "do " + id,
		Mode:        "auto",
		State:       state,
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
		// Production creates a session straight into the backlog, so its queue
		// age starts with it.
		QueuedAt: createdAt,
	}
}

// idsOf reduces a session slice to its ids, for order assertions.
func idsOf(sessions []*session.Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}

// DescribeStore runs the shared Store conformance suite against the store the
// factory produces. Run against both memStore and the SQLite store.
func DescribeStore(name string, newStore func() session.Store) bool {
	return Describe("Store conformance: "+name, func() {
		var (
			store session.Store
			ctx   context.Context
			base  time.Time
		)

		BeforeEach(func() {
			store = newStore()
			ctx = context.Background()
			base = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
			DeferCleanup(func() { Expect(store.Close()).To(Succeed()) })
		})

		It("round-trips a session including cost JSON and PR URL", func() {
			s := makeSession("s1", "proj", session.StateRunning, base)
			s.Cost = cost.Report{
				InputTokens:  10,
				OutputTokens: 20,
				TotalUSD:     1.5,
				PerModel: map[string]cost.ModelCost{
					"anthropic/opus": {ModelID: "opus", InputTokens: 10, OutputTokens: 20, TotalUSD: 1.5},
				},
			}
			Expect(store.CreateSession(ctx, s)).To(Succeed())

			s.PullRequestURL = "https://example.com/pr/1"
			s.State = session.StateCompleted
			Expect(store.UpdateSession(ctx, s)).To(Succeed())

			got, err := store.GetSession(ctx, "s1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ProjectName).To(Equal("proj"))
			Expect(got.State).To(Equal(session.StateCompleted))
			Expect(got.PullRequestURL).To(Equal("https://example.com/pr/1"))
			Expect(got.Cost.TotalUSD).To(Equal(1.5))
			Expect(got.Cost.PerModel).To(HaveKey("anthropic/opus"))
			Expect(got.Cost.PerModel["anthropic/opus"].OutputTokens).To(Equal(int64(20)))
			Expect(got.CreatedAt.Equal(base)).To(BeTrue())
		})

		It("round-trips the pull request fields", func() {
			s := makeSession("s1", "proj", session.StateRunning, base)
			s.PRRef = "42"
			s.HeadBranch = "kvarn/add-a-helper"
			s.BaseBranch = "main"
			s.ParentSessionID = "parent-1"
			Expect(store.CreateSession(ctx, s)).To(Succeed())

			got, err := store.GetSession(ctx, "s1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.PRRef).To(Equal("42"))
			Expect(got.HeadBranch).To(Equal("kvarn/add-a-helper"))
			Expect(got.BaseBranch).To(Equal("main"))
			Expect(got.ParentSessionID).To(Equal("parent-1"))

			// A fresh job learns its PR at submission time, so the update path
			// has to persist these too.
			s.PRRef = "43"
			s.HeadBranch = "kvarn/renamed"
			Expect(store.UpdateSession(ctx, s)).To(Succeed())
			got, err = store.GetSession(ctx, "s1")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.PRRef).To(Equal("43"))
			Expect(got.HeadBranch).To(Equal("kvarn/renamed"))
		})

		It("filters by PR ref, alone and combined with active-only", func() {
			onPR := makeSession("on-pr", "proj", session.StateCompleted, base.Add(1*time.Minute))
			onPR.PRRef = "42"
			running := makeSession("running", "proj", session.StateRunning, base.Add(2*time.Minute))
			running.PRRef = "42"
			otherPR := makeSession("other-pr", "proj", session.StateRunning, base.Add(3*time.Minute))
			otherPR.PRRef = "43"
			noPR := makeSession("no-pr", "proj", session.StateRunning, base.Add(4*time.Minute))
			for _, s := range []*session.Session{onPR, running, otherPR, noPR} {
				Expect(store.CreateSession(ctx, s)).To(Succeed())
			}

			got, err := store.ListSessions(ctx, session.SessionFilter{Project: "proj", PRRef: "42"})
			Expect(err).NotTo(HaveOccurred())
			ids := []string{}
			for _, s := range got {
				ids = append(ids, s.ID)
			}
			Expect(ids).To(Equal([]string{"running", "on-pr"})) // newest first

			active, err := store.ListSessions(ctx, session.SessionFilter{
				Project: "proj", PRRef: "42", ActiveOnly: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(HaveLen(1))
			Expect(active[0].ID).To(Equal("running"))

			// An empty PRRef filter matches every session, including those
			// with no pull request.
			all, err := store.ListSessions(ctx, session.SessionFilter{Project: "proj"})
			Expect(err).NotTo(HaveOccurred())
			Expect(all).To(HaveLen(4))
		})

		It("filters by state, mode and creation time", func() {
			pending := makeSession("pending", "proj", session.StatePending, base.Add(1*time.Minute))
			running := makeSession("running", "proj", session.StateRunning, base.Add(2*time.Minute))
			failed := makeSession("failed", "proj", session.StateFailed, base.Add(3*time.Minute))
			failed.Mode = "feedback"
			for _, s := range []*session.Session{pending, running, failed} {
				Expect(store.CreateSession(ctx, s)).To(Succeed())
			}

			byState, err := store.ListSessions(ctx, session.SessionFilter{
				States: []session.State{session.StatePending, session.StateFailed},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(byState)).To(Equal([]string{"failed", "pending"})) // newest first

			// States and ActiveOnly are ANDed rather than one overriding the
			// other, so a terminal state named explicitly still drops out.
			activeOfThose, err := store.ListSessions(ctx, session.SessionFilter{
				States:     []session.State{session.StatePending, session.StateFailed},
				ActiveOnly: true,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(activeOfThose)).To(Equal([]string{"pending"}))

			byMode, err := store.ListSessions(ctx, session.SessionFilter{Mode: "feedback"})
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(byMode)).To(Equal([]string{"failed"}))

			// Strictly after: the session created exactly at the bound is out.
			since, err := store.ListSessions(ctx, session.SessionFilter{
				CreatedAfter: base.Add(2 * time.Minute),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(since)).To(Equal([]string{"failed"}))
		})

		It("returns not-found for an unknown session", func() {
			_, err := store.GetSession(ctx, "missing")
			Expect(err).To(MatchError(ContainSubstring("not found")))
		})

		It("assigns per-session monotonic seqs with independent counters", func() {
			a := makeSession("a", "p", session.StateRunning, base)
			b := makeSession("b", "p", session.StateRunning, base)
			Expect(store.CreateSession(ctx, a)).To(Succeed())
			Expect(store.CreateSession(ctx, b)).To(Succeed())

			for i := 1; i <= 3; i++ {
				ev, err := store.AppendEvent(ctx, "a", "agent_message", []byte(fmt.Sprintf(`{"n":%d}`, i)))
				Expect(err).NotTo(HaveOccurred())
				Expect(ev.Seq).To(Equal(int64(i)))
			}
			ev, err := store.AppendEvent(ctx, "b", "agent_message", []byte(`{}`))
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Seq).To(Equal(int64(1)))

			maxA, err := store.MaxSeq(ctx, "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(maxA).To(Equal(int64(3)))
			maxB, err := store.MaxSeq(ctx, "b")
			Expect(err).NotTo(HaveOccurred())
			Expect(maxB).To(Equal(int64(1)))
		})

		It("lists events in seq order, honoring afterSeq and limit", func() {
			s := makeSession("s", "p", session.StateRunning, base)
			Expect(store.CreateSession(ctx, s)).To(Succeed())
			for i := 1; i <= 5; i++ {
				_, err := store.AppendEvent(ctx, "s", "agent_message", []byte(fmt.Sprintf(`{"n":%d}`, i)))
				Expect(err).NotTo(HaveOccurred())
			}

			all, err := store.ListEvents(ctx, "s", 0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(all).To(HaveLen(5))
			for i, ev := range all {
				Expect(ev.Seq).To(Equal(int64(i + 1)))
			}

			after, err := store.ListEvents(ctx, "s", 2, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(after).To(HaveLen(3))
			Expect(after[0].Seq).To(Equal(int64(3)))

			limited, err := store.ListEvents(ctx, "s", 0, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(limited).To(HaveLen(2))
			Expect(limited[1].Seq).To(Equal(int64(2)))
		})

		It("filters by project, active-only, limit and cursor", func() {
			// created_at ascending; listing is created_at DESC, id DESC.
			s1 := makeSession("id1", "alpha", session.StateCompleted, base.Add(1*time.Minute))
			s2 := makeSession("id2", "beta", session.StateRunning, base.Add(2*time.Minute))
			s3 := makeSession("id3", "alpha", session.StateRunning, base.Add(3*time.Minute))
			for _, s := range []*session.Session{s1, s2, s3} {
				Expect(store.CreateSession(ctx, s)).To(Succeed())
			}

			byProject, err := store.ListSessions(ctx, session.SessionFilter{Project: "alpha"})
			Expect(err).NotTo(HaveOccurred())
			Expect(byProject).To(HaveLen(2))
			Expect(byProject[0].ID).To(Equal("id3")) // newest first

			active, err := store.ListSessions(ctx, session.SessionFilter{ActiveOnly: true})
			Expect(err).NotTo(HaveOccurred())
			ids := []string{}
			for _, s := range active {
				ids = append(ids, s.ID)
			}
			Expect(ids).To(ConsistOf("id2", "id3"))

			page1, err := store.ListSessions(ctx, session.SessionFilter{Limit: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(page1).To(HaveLen(2))
			Expect(page1[0].ID).To(Equal("id3"))
			Expect(page1[1].ID).To(Equal("id2"))

			last := page1[len(page1)-1]
			page2, err := store.ListSessions(ctx, session.SessionFilter{
				Limit:          2,
				AfterCreatedAt: last.CreatedAt,
				AfterID:        last.ID,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(page2).To(HaveLen(1))
			Expect(page2[0].ID).To(Equal("id1"))
		})

		It("requeues restartable sessions and fails the rest, appending state_change to each", func() {
			cloning := makeSession("cloning", "p", session.StateCloning, base)
			running := makeSession("run", "p", session.StateRunning, base)
			submitting := makeSession("submit", "p", session.StateSubmitting, base)
			done := makeSession("done", "p", session.StateCompleted, base)
			for _, s := range []*session.Session{cloning, running, submitting, done} {
				Expect(store.CreateSession(ctx, s)).To(Succeed())
			}

			res, err := store.ReconcileStartup(ctx, session.ReconcileOpts{
				RequeueMessage: "requeued", FailError: "orchestrator restarted",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Requeued).To(ConsistOf("cloning"))
			// A run that had spent budget, and one that may already have pushed
			// a branch, are both too late to start over.
			Expect(res.Failed).To(ConsistOf("run", "submit"))

			back, err := store.GetSession(ctx, "cloning")
			Expect(err).NotTo(HaveOccurred())
			Expect(back.State).To(Equal(session.StatePending))
			Expect(back.Attempts).To(Equal(1))
			Expect(back.Error).To(BeEmpty())
			Expect(back.QueuedAt).To(BeTemporally(">", base))

			failed, err := store.GetSession(ctx, "run")
			Expect(err).NotTo(HaveOccurred())
			Expect(failed.State).To(Equal(session.StateFailed))
			Expect(failed.Error).To(Equal("orchestrator restarted"))

			for _, id := range []string{"cloning", "run", "submit"} {
				evs, err := store.ListEvents(ctx, id, 0, 0)
				Expect(err).NotTo(HaveOccurred())
				Expect(evs).To(HaveLen(1))
				Expect(evs[0].Kind).To(Equal("state_change"))
			}

			// The terminal session is untouched, and the requeued one is now
			// pending so a second pass leaves it in the backlog rather than
			// charging it another attempt.
			res2, err := store.ReconcileStartup(ctx, session.ReconcileOpts{FailError: "again"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res2.Requeued).To(BeEmpty())
			Expect(res2.Failed).To(BeEmpty())

			doneEvs, err := store.ListEvents(ctx, "done", 0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(doneEvs).To(BeEmpty())
		})

		It("fails a restartable session that has used up its attempts", func() {
			s := makeSession("looper", "p", session.StateSetup, base)
			s.Attempts = 3
			Expect(store.CreateSession(ctx, s)).To(Succeed())

			res, err := store.ReconcileStartup(ctx, session.ReconcileOpts{
				MaxAttempts: 3, FailError: "orchestrator restarted",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Requeued).To(BeEmpty())
			Expect(res.Failed).To(ConsistOf("looper"))

			got, err := store.GetSession(ctx, "looper")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(session.StateFailed))
			Expect(got.Error).To(ContainSubstring("gave up after 3 attempts"))
		})

		It("carries spend into a requeued run so a retry cannot recharge the cap", func() {
			s := makeSession("spent", "p", session.StateSetup, base)
			s.Cost = cost.Report{InputTokens: 100, TotalUSD: 2.5}
			Expect(store.CreateSession(ctx, s)).To(Succeed())

			_, err := store.ReconcileStartup(ctx, session.ReconcileOpts{RequeueMessage: "requeued"})
			Expect(err).NotTo(HaveOccurred())

			got, err := store.GetSession(ctx, "spent")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(session.StatePending))
			Expect(got.Cost.TotalUSD).To(Equal(2.5))
		})

		Describe("backlog", func() {
			// queued builds a pending session that entered the backlog at the
			// given time with the given priority.
			queued := func(id string, project string, priority int, at time.Time) *session.Session {
				s := makeSession(id, project, session.StatePending, at)
				s.Priority = priority
				s.QueuedAt = at
				return s
			}

			It("counts and lists only pending sessions", func() {
				Expect(store.CreateSession(ctx, queued("a", "p", 0, base))).To(Succeed())
				Expect(store.CreateSession(ctx, makeSession("b", "p", session.StateRunning, base))).To(Succeed())

				n, err := store.CountPending(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(n).To(Equal(1))

				got, err := store.ListPending(ctx, session.PendingQuery{Now: base})
				Expect(err).NotTo(HaveOccurred())
				Expect(idsOf(got)).To(Equal([]string{"a"}))
			})

			It("orders by priority first and arrival second", func() {
				Expect(store.CreateSession(ctx, queued("old-low", "p", 0, base))).To(Succeed())
				Expect(store.CreateSession(ctx, queued("new-high", "p", 5, base.Add(time.Minute)))).To(Succeed())
				Expect(store.CreateSession(ctx, queued("old-high", "p", 5, base))).To(Succeed())

				got, err := store.ListPending(ctx, session.PendingQuery{Now: base.Add(time.Minute)})
				Expect(err).NotTo(HaveOccurred())
				Expect(idsOf(got)).To(Equal([]string{"old-high", "new-high", "old-low"}))
			})

			It("ages a waiting entry up to, but never past, the highest priority queued", func() {
				Expect(store.CreateSession(ctx, queued("waiting", "p", 0, base))).To(Succeed())
				Expect(store.CreateSession(ctx, queued("important", "p", 2, base.Add(time.Hour)))).To(Succeed())

				q := session.PendingQuery{Now: base.Add(time.Hour), AgeStep: 10 * time.Minute}
				got, err := store.ListPending(ctx, q)
				Expect(err).NotTo(HaveOccurred())
				// Six age steps would put it at 6, but the clamp holds it at 2
				// and the tie falls to arrival — which the waiting entry wins.
				Expect(idsOf(got)).To(Equal([]string{"waiting", "important"}))
			})

			It("honours the limit", func() {
				for i := range 3 {
					Expect(store.CreateSession(ctx, queued(fmt.Sprintf("s%d", i), "p", 0, base.Add(time.Duration(i)*time.Second)))).To(Succeed())
				}
				got, err := store.ListPending(ctx, session.PendingQuery{Now: base, Limit: 2})
				Expect(err).NotTo(HaveOccurred())
				Expect(got).To(HaveLen(2))
			})

			It("reorders a pending entry and refuses one that has been dispatched", func() {
				Expect(store.CreateSession(ctx, queued("waiting", "p", 1, base))).To(Succeed())
				Expect(store.CreateSession(ctx, queued("ahead", "p", 5, base))).To(Succeed())

				previous, ok, err := store.UpdatePendingPriority(ctx, "waiting", 9)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue())
				Expect(previous).To(Equal(1))

				got, err := store.ListPending(ctx, session.PendingQuery{Now: base})
				Expect(err).NotTo(HaveOccurred())
				Expect(idsOf(got)).To(Equal([]string{"waiting", "ahead"}))

				// Once claimed the value orders nothing, so the write is
				// refused rather than silently applied.
				claimed, _, err := store.TransitionPending(ctx, "ahead", session.PendingTransition{
					State: session.StateQueued,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(claimed).To(BeTrue())

				_, ok, err = store.UpdatePendingPriority(ctx, "ahead", 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeFalse())

				after, err := store.GetSession(ctx, "ahead")
				Expect(err).NotTo(HaveOccurred())
				Expect(after.Priority).To(Equal(5))
			})

			It("lets exactly one caller transition a pending session", func() {
				Expect(store.CreateSession(ctx, queued("race", "p", 0, base))).To(Succeed())

				won, ev, err := store.TransitionPending(ctx, "race", session.PendingTransition{
					State: session.StateQueued, Message: "dispatched",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(won).To(BeTrue())
				Expect(ev.Kind).To(Equal("state_change"))
				Expect(ev.Seq).To(Equal(int64(1)))

				// The loser is told it lost rather than handed an error.
				won2, _, err := store.TransitionPending(ctx, "race", session.PendingTransition{
					State: session.StateCancelled, Message: "too late",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(won2).To(BeFalse())

				got, err := store.GetSession(ctx, "race")
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(session.StateQueued))
				Expect(got.Message).To(Equal("dispatched"))
			})

			It("reports a miss for a session that was never pending", func() {
				Expect(store.CreateSession(ctx, makeSession("busy", "p", session.StateRunning, base))).To(Succeed())
				won, _, err := store.TransitionPending(ctx, "busy", session.PendingTransition{State: session.StateCancelled})
				Expect(err).NotTo(HaveOccurred())
				Expect(won).To(BeFalse())
			})

			It("expires entries queued before the cutoff and leaves the rest", func() {
				Expect(store.CreateSession(ctx, queued("stale", "p", 0, base.Add(-48*time.Hour)))).To(Succeed())
				Expect(store.CreateSession(ctx, queued("fresh", "p", 0, base))).To(Succeed())

				ids, err := store.ExpirePending(ctx, base.Add(-24*time.Hour), "waited too long")
				Expect(err).NotTo(HaveOccurred())
				Expect(ids).To(ConsistOf("stale"))

				got, err := store.GetSession(ctx, "stale")
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(session.StateFailed))
				Expect(got.Error).To(Equal("waited too long"))

				n, err := store.CountPending(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(n).To(Equal(1))
			})
		})

		It("treats a cancelled session as terminal", func() {
			cancelled := makeSession("cancelled", "p", session.StateCancelled, base.Add(-48*time.Hour))
			Expect(store.CreateSession(ctx, cancelled)).To(Succeed())

			active, err := store.ListSessions(ctx, session.SessionFilter{ActiveOnly: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeEmpty())

			// Startup reconciliation leaves it alone rather than flipping it to
			// failed, and retention prunes it like any other finished session.
			res, err := store.ReconcileStartup(ctx, session.ReconcileOpts{FailError: "orchestrator restarted"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Requeued).To(BeEmpty())
			Expect(res.Failed).To(BeEmpty())

			n, err := store.PruneTerminalBefore(ctx, base.Add(-24*time.Hour))
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))
		})

		It("yields a gapless seq range under concurrent AppendEvent", func() {
			s := makeSession("s", "p", session.StateRunning, base)
			Expect(store.CreateSession(ctx, s)).To(Succeed())

			const n = 50
			var wg sync.WaitGroup
			seqs := make([]int64, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					defer GinkgoRecover()
					ev, err := store.AppendEvent(ctx, "s", "agent_message", []byte(`{}`))
					Expect(err).NotTo(HaveOccurred())
					seqs[i] = ev.Seq
				}(i)
			}
			wg.Wait()

			sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
			for i := 0; i < n; i++ {
				Expect(seqs[i]).To(Equal(int64(i + 1)), "expected gapless seq range")
			}
		})

		It("prunes only terminal sessions older than the cutoff and cascades events", func() {
			oldDone := makeSession("oldDone", "p", session.StateCompleted, base.Add(-48*time.Hour))
			newDone := makeSession("newDone", "p", session.StateCompleted, base)
			oldRunning := makeSession("oldRunning", "p", session.StateRunning, base.Add(-48*time.Hour))
			for _, s := range []*session.Session{oldDone, newDone, oldRunning} {
				Expect(store.CreateSession(ctx, s)).To(Succeed())
			}
			_, err := store.AppendEvent(ctx, "oldDone", "agent_message", []byte(`{}`))
			Expect(err).NotTo(HaveOccurred())

			cutoff := base.Add(-24 * time.Hour)
			n, err := store.PruneTerminalBefore(ctx, cutoff)
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(1))

			_, err = store.GetSession(ctx, "oldDone")
			Expect(err).To(HaveOccurred())
			// Its events cascaded away.
			evs, err := store.ListEvents(ctx, "oldDone", 0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(evs).To(BeEmpty())

			// Newer terminal + old non-terminal survive.
			_, err = store.GetSession(ctx, "newDone")
			Expect(err).NotTo(HaveOccurred())
			_, err = store.GetSession(ctx, "oldRunning")
			Expect(err).NotTo(HaveOccurred())
		})
	})
}

var _ = DescribeStore("memStore", func() session.Store {
	return session.NewMemStore()
})

var _ = DescribeStore("sqlite", func() session.Store {
	store, err := sqlitestore.New(filepath.Join(GinkgoT().TempDir(), "sessions.db"))
	Expect(err).NotTo(HaveOccurred())
	return store
})
