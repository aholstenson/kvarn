package sandbox_test

import (
	"sync"
	"sync/atomic"

	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Session.Close", func() {
	It("executes each closer exactly once when Close is called concurrently", func() {
		// Build a session via the exported test helper so we can register a
		// closer without going through the full VM boot path.
		sess := sandbox.NewTestSession()

		var callCount atomic.Int64
		sess.AddCloserForTest(func() {
			callCount.Add(1)
		})

		// Fire Close from many goroutines simultaneously to expose any race.
		const goroutines = 50
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				sess.Close()
			}()
		}
		wg.Wait()

		// The closer must have been called exactly once.
		Expect(callCount.Load()).To(Equal(int64(1)))
	})

	It("executes closers in reverse registration order", func() {
		sess := sandbox.NewTestSession()

		var order []int
		var mu sync.Mutex
		for i := 0; i < 3; i++ {
			i := i
			sess.AddCloserForTest(func() {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
			})
		}

		sess.Close()

		Expect(order).To(Equal([]int{2, 1, 0}))
	})

	It("is idempotent: second Close after the first is a no-op", func() {
		sess := sandbox.NewTestSession()

		var callCount atomic.Int64
		sess.AddCloserForTest(func() {
			callCount.Add(1)
		})

		sess.Close()
		sess.Close()

		Expect(callCount.Load()).To(Equal(int64(1)))
	})
})
