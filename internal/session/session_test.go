package session_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/session"
)

var _ = Describe("MemoryManager", func() {
	var (
		mgr *session.MemoryManager
		ctx context.Context
	)

	BeforeEach(func() {
		mgr = session.NewMemoryManager()
		ctx = context.Background()
	})

	It("creates a session with pending state", func() {
		sess, err := mgr.Create(ctx, "my-project", "do something")
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.ID).NotTo(BeEmpty())
		Expect(sess.ProjectName).To(Equal("my-project"))
		Expect(sess.Prompt).To(Equal("do something"))
		Expect(sess.State).To(Equal(session.StatePending))
	})

	It("gets a session by ID", func() {
		created, err := mgr.Create(ctx, "proj", "prompt")
		Expect(err).NotTo(HaveOccurred())

		got, err := mgr.Get(ctx, created.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ID).To(Equal(created.ID))
		Expect(got.ProjectName).To(Equal("proj"))
	})

	It("returns error for unknown session", func() {
		_, err := mgr.Get(ctx, "nonexistent")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
	})

	It("lists sessions", func() {
		_, err := mgr.Create(ctx, "a", "p1")
		Expect(err).NotTo(HaveOccurred())
		_, err = mgr.Create(ctx, "b", "p2")
		Expect(err).NotTo(HaveOccurred())

		sessions, err := mgr.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(sessions).To(HaveLen(2))
	})

	It("updates session state", func() {
		sess, err := mgr.Create(ctx, "proj", "prompt")
		Expect(err).NotTo(HaveOccurred())

		err = mgr.UpdateState(ctx, sess.ID, session.StateCloning, "Cloning repo")
		Expect(err).NotTo(HaveOccurred())

		got, err := mgr.Get(ctx, sess.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(session.StateCloning))
		Expect(got.Message).To(Equal("Cloning repo"))
	})

	It("fails a session", func() {
		sess, err := mgr.Create(ctx, "proj", "prompt")
		Expect(err).NotTo(HaveOccurred())

		err = mgr.Fail(ctx, sess.ID, fmt.Errorf("something broke"))
		Expect(err).NotTo(HaveOccurred())

		got, err := mgr.Get(ctx, sess.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.State).To(Equal(session.StateFailed))
		Expect(got.Error).To(Equal("something broke"))
	})

	It("returns error when updating unknown session", func() {
		err := mgr.UpdateState(ctx, "bad-id", session.StateRunning, "")
		Expect(err).To(HaveOccurred())
	})

	Describe("Watch", func() {
		It("receives state updates", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt")
			Expect(err).NotTo(HaveOccurred())

			watchCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, err := mgr.Watch(watchCtx, sess.ID)
			Expect(err).NotTo(HaveOccurred())

			// Update state and check we receive it.
			err = mgr.UpdateState(ctx, sess.ID, session.StateCloning, "cloning")
			Expect(err).NotTo(HaveOccurred())

			var event session.Event
			Eventually(ch).Should(Receive(&event))
			stateChange, ok := event.(session.StateChangeEvent)
			Expect(ok).To(BeTrue())
			Expect(stateChange.Session.State).To(Equal(session.StateCloning))
		})

		It("closes channel on terminal state", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt")
			Expect(err).NotTo(HaveOccurred())

			watchCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, err := mgr.Watch(watchCtx, sess.ID)
			Expect(err).NotTo(HaveOccurred())

			err = mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")
			Expect(err).NotTo(HaveOccurred())

			// Drain the update.
			Eventually(ch).Should(Receive())

			// Channel should be closed.
			Eventually(ch).Should(BeClosed())
		})

		It("returns closed channel with final state for already-terminal session", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt")
			Expect(err).NotTo(HaveOccurred())

			err = mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")
			Expect(err).NotTo(HaveOccurred())

			ch, err := mgr.Watch(ctx, sess.ID)
			Expect(err).NotTo(HaveOccurred())

			var event session.Event
			Eventually(ch).Should(Receive(&event))
			stateChange, ok := event.(session.StateChangeEvent)
			Expect(ok).To(BeTrue())
			Expect(stateChange.Session.State).To(Equal(session.StateCompleted))

			Eventually(ch).Should(BeClosed())
		})

		It("returns error for unknown session", func() {
			_, err := mgr.Watch(ctx, "nonexistent")
			Expect(err).To(HaveOccurred())
		})

		It("handles concurrent access safely", func() {
			sess, err := mgr.Create(ctx, "proj", "prompt")
			Expect(err).NotTo(HaveOccurred())

			var wg sync.WaitGroup
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					mgr.Get(ctx, sess.ID)
				}()
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(10 * time.Millisecond)
				mgr.UpdateState(ctx, sess.ID, session.StateCompleted, "done")
			}()

			wg.Wait()

			got, err := mgr.Get(ctx, sess.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(session.StateCompleted))
		})
	})
})
