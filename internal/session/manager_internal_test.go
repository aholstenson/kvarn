package session

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// MaxPending exposes the disconnect-on-lag threshold to the external test
// package, so a test that has to stay on one side of it tracks the constant
// instead of a copy of its value.
const MaxPending = maxPending

// subscriberCount reports how many live subscribers are registered for id.
func (m *manager) subscriberCount(id string) int {
	m.hub.mu.Lock()
	defer m.hub.mu.Unlock()
	return len(m.hub.subs[id])
}

var _ = Describe("manager subscriber lifecycle", func() {
	It("removes the subscriber on ctx cancel, leaving no leak", func() {
		mgr := NewManager(newMemStore())
		ctx := context.Background()
		sess, err := mgr.Create(ctx, CreateParams{ProjectName: "proj", Prompt: "prompt", Mode: "auto"})
		Expect(err).NotTo(HaveOccurred())

		watchCtx, cancel := context.WithCancel(ctx)
		ch, err := mgr.Watch(watchCtx, sess.ID, 0)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() int { return mgr.subscriberCount(sess.ID) }).Should(Equal(1))

		cancel()
		// The feeder unregisters and closes the channel.
		Eventually(ch).Should(BeClosed())
		Eventually(func() int { return mgr.subscriberCount(sess.ID) }, 2*time.Second).Should(Equal(0))
	})
})
