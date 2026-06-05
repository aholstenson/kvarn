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
	}
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

		It("reconciles non-terminal sessions, appends state_change, and is idempotent", func() {
			running := makeSession("run", "p", session.StateRunning, base)
			done := makeSession("done", "p", session.StateCompleted, base)
			Expect(store.CreateSession(ctx, running)).To(Succeed())
			Expect(store.CreateSession(ctx, done)).To(Succeed())

			ids, err := store.ReconcileNonTerminal(ctx, "orchestrator restarted")
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(ConsistOf("run"))

			got, err := store.GetSession(ctx, "run")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(session.StateFailed))
			Expect(got.Error).To(Equal("orchestrator restarted"))

			evs, err := store.ListEvents(ctx, "run", 0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(evs).To(HaveLen(1))
			Expect(evs[0].Kind).To(Equal("state_change"))

			// The terminal session is untouched and re-running is a no-op.
			ids2, err := store.ReconcileNonTerminal(ctx, "again")
			Expect(err).NotTo(HaveOccurred())
			Expect(ids2).To(BeEmpty())

			doneEvs, err := store.ListEvents(ctx, "done", 0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(doneEvs).To(BeEmpty())
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
