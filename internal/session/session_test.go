package session_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/session"
)

// drainClosed reads from ch until it is closed or the timeout elapses,
// returning everything collected. The second return is true if the channel
// closed within the timeout.
func drainClosed(ch <-chan session.WatchEvent, timeout time.Duration) ([]session.WatchEvent, bool) {
	var out []session.WatchEvent
	deadline := time.After(timeout)
	for {
		select {
		case we, ok := <-ch:
			if !ok {
				return out, true
			}
			out = append(out, we)
		case <-deadline:
			return out, false
		}
	}
}

// durableSeqs extracts the non-zero (durable) sequence numbers in order.
func durableSeqs(events []session.WatchEvent) []int64 {
	var seqs []int64
	for _, we := range events {
		if we.Seq != 0 {
			seqs = append(seqs, we.Seq)
		}
	}
	return seqs
}

var _ = Describe("Manager", func() {
	var (
		mgr session.Manager
		ctx context.Context
	)

	BeforeEach(func() {
		mgr = session.NewManager(session.NewMemStore())
		ctx = context.Background()
	})

	It("creates a session with pending state", func() {
		sess, err := mgr.Create(ctx, "my-project", "do something", "auto")
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.ID).NotTo(BeEmpty())
		Expect(sess.ProjectName).To(Equal("my-project"))
		Expect(sess.State).To(Equal(session.StatePending))
	})

	It("gets and lists sessions", func() {
		a, err := mgr.Create(ctx, "a", "p1", "auto")
		Expect(err).NotTo(HaveOccurred())
		_, err = mgr.Create(ctx, "b", "p2", "auto")
		Expect(err).NotTo(HaveOccurred())

		got, err := mgr.Get(ctx, a.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ID).To(Equal(a.ID))

		all, err := mgr.List(ctx, session.SessionFilter{})
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(2))
	})

	It("updates state and fails sessions", func() {
		sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
		Expect(err).NotTo(HaveOccurred())

		Expect(mgr.UpdateState(ctx, sess.ID, session.StateCloning, "Cloning")).To(Succeed())
		got, err := mgr.Get(ctx, sess.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(session.StateCloning))
		Expect(got.Message).To(Equal("Cloning"))

		Expect(mgr.Fail(ctx, sess.ID, fmt.Errorf("boom"))).To(Succeed())
		got, err = mgr.Get(ctx, sess.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(session.StateFailed))
		Expect(got.Error).To(Equal("boom"))
	})

	It("persists the pull request URL via SetPullRequest", func() {
		sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
		Expect(err).NotTo(HaveOccurred())

		Expect(mgr.SetPullRequest(ctx, sess.ID, "https://example.com/pr/7", 7, "feature/x")).To(Succeed())
		got, err := mgr.Get(ctx, sess.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.PullRequestURL).To(Equal("https://example.com/pr/7"))
	})

	It("returns error for unknown session", func() {
		_, err := mgr.Get(ctx, "nope")
		Expect(err).To(HaveOccurred())
		_, err = mgr.Watch(ctx, "nope", 0)
		Expect(err).To(HaveOccurred())
	})

	Describe("Watch", func() {
		It("replays history from seq 0 then streams live with no gap or dup", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())

			// Pre-Watch durable history.
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateCloning, "1")).To(Succeed())
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateRunning, "2")).To(Succeed())

			watchCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			ch, err := mgr.Watch(watchCtx, sess.ID, 0)
			Expect(err).NotTo(HaveOccurred())

			// Live durable events after Watch.
			Expect(mgr.EmitEvent(ctx, sess.ID, session.AgentMessageEvent{SessionID: sess.ID, Text: "hi"})).To(Succeed())
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")).To(Succeed())

			events, closed := drainClosed(ch, 2*time.Second)
			Expect(closed).To(BeTrue())
			seqs := durableSeqs(events)
			// Contiguous 1..N starting at 1.
			Expect(seqs).NotTo(BeEmpty())
			for i, s := range seqs {
				Expect(s).To(Equal(int64(i + 1)))
			}
		})

		It("resumes from a cursor, skipping events <= fromSeq", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateCloning, "1")).To(Succeed())
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateRunning, "2")).To(Succeed())
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "3")).To(Succeed())

			ch, err := mgr.Watch(ctx, sess.ID, 2)
			Expect(err).NotTo(HaveOccurred())
			events, closed := drainClosed(ch, 2*time.Second)
			Expect(closed).To(BeTrue())
			seqs := durableSeqs(events)
			Expect(seqs).To(Equal([]int64{3}))
		})

		It("returns history then closes for an already-terminal session", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")).To(Succeed())

			ch, err := mgr.Watch(ctx, sess.ID, 0)
			Expect(err).NotTo(HaveOccurred())
			events, closed := drainClosed(ch, 2*time.Second)
			Expect(closed).To(BeTrue())
			Expect(durableSeqs(events)).To(Equal([]int64{1}))
			last := events[len(events)-1].Event.(session.StateChangeEvent)
			Expect(last.Session.State).To(Equal(session.StateCompleted))
		})

		It("delivers ephemeral events live with seq 0 and never replays them", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())

			ch, err := mgr.Watch(ctx, sess.ID, 0)
			Expect(err).NotTo(HaveOccurred())

			Expect(mgr.EmitEvent(ctx, sess.ID, session.ConsoleOutputEvent{SessionID: sess.ID, Output: "log"})).To(Succeed())

			var got session.WatchEvent
			Eventually(ch).Should(Receive(&got))
			Expect(got.Seq).To(Equal(int64(0)))
			_, ok := got.Event.(session.ConsoleOutputEvent)
			Expect(ok).To(BeTrue())

			// Ephemeral events are not in the durable log.
			polled, err := mgr.ListEvents(ctx, sess.ID, 0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(polled).To(BeEmpty())
		})

		It("disconnects a lagging subscriber and recovers the gap on re-Watch", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())

			ch, err := mgr.Watch(ctx, sess.ID, 0)
			Expect(err).NotTo(HaveOccurred())

			// Flood durable events without reading; the subscriber should be
			// disconnected once it falls too far behind.
			const n = 300
			for i := 0; i < n; i++ {
				Expect(mgr.EmitEvent(ctx, sess.ID, session.AgentMessageEvent{SessionID: sess.ID, Text: "x"})).To(Succeed())
			}
			Expect(mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")).To(Succeed())

			first, closed := drainClosed(ch, 2*time.Second)
			Expect(closed).To(BeTrue())
			firstSeqs := durableSeqs(first)
			Expect(len(firstSeqs)).To(BeNumerically("<", n+1), "lagging watcher should be cut short")

			// Reconnect from the last seen seq and replay the gap to terminal.
			var lastSeen int64
			if len(firstSeqs) > 0 {
				lastSeen = firstSeqs[len(firstSeqs)-1]
			}
			ch2, err := mgr.Watch(ctx, sess.ID, lastSeen)
			Expect(err).NotTo(HaveOccurred())
			second, closed2 := drainClosed(ch2, 3*time.Second)
			Expect(closed2).To(BeTrue())
			secondSeqs := durableSeqs(second)
			Expect(secondSeqs).NotTo(BeEmpty())
			// No gap: first delivered seq is exactly lastSeen+1, contiguous to the end.
			Expect(secondSeqs[0]).To(Equal(lastSeen + 1))
			for i := 1; i < len(secondSeqs); i++ {
				Expect(secondSeqs[i]).To(Equal(secondSeqs[i-1] + 1))
			}
			Expect(secondSeqs[len(secondSeqs)-1]).To(Equal(int64(n + 1)))
		})

		It("delivers strictly increasing contiguous seqs when watching during an append burst", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt", "auto")
			Expect(err).NotTo(HaveOccurred())

			ch, err := mgr.Watch(ctx, sess.ID, 0)
			Expect(err).NotTo(HaveOccurred())

			const n = 100
			go func() {
				defer GinkgoRecover()
				for i := 0; i < n; i++ {
					Expect(mgr.EmitEvent(ctx, sess.ID, session.AgentMessageEvent{SessionID: sess.ID, Text: "x"})).To(Succeed())
				}
				Expect(mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")).To(Succeed())
			}()

			events, closed := drainClosed(ch, 5*time.Second)
			Expect(closed).To(BeTrue())
			seqs := durableSeqs(events)
			Expect(seqs[0]).To(Equal(int64(1)))
			for i := 1; i < len(seqs); i++ {
				Expect(seqs[i]).To(Equal(seqs[i-1] + 1))
			}
			Expect(seqs[len(seqs)-1]).To(Equal(int64(n + 1)))
		})
	})
})
