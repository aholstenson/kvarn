//go:build unix

package atomicfile_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WithLock", func() {
	It("serializes concurrent holders on the same path", func() {
		path := filepath.Join(GinkgoT().TempDir(), "data.toml")

		var inside atomic.Int32
		var maxConcurrent atomic.Int32
		var wg sync.WaitGroup

		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				Expect(atomicfile.WithLock(path, func() error {
					n := inside.Add(1)
					for {
						cur := maxConcurrent.Load()
						if n <= cur || maxConcurrent.CompareAndSwap(cur, n) {
							break
						}
					}
					time.Sleep(10 * time.Millisecond)
					inside.Add(-1)
					return nil
				})).To(Succeed())
			}()
		}
		wg.Wait()

		Expect(maxConcurrent.Load()).To(Equal(int32(1)))
	})

	It("releases the lock on fn error so subsequent calls succeed", func() {
		path := filepath.Join(GinkgoT().TempDir(), "data.toml")

		boom := &simpleErr{"boom"}
		Expect(atomicfile.WithLock(path, func() error { return boom })).
			To(MatchError(boom))

		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			Expect(atomicfile.WithLock(path, func() error { return nil })).To(Succeed())
			close(done)
		}()
		Eventually(done, "1s").Should(BeClosed())
	})
})

var _ = Describe("Acquire", func() {
	It("lets shared holders coexist", func() {
		path := filepath.Join(GinkgoT().TempDir(), "data.toml")

		first, err := atomicfile.Acquire(context.Background(), path, false)
		Expect(err).NotTo(HaveOccurred())
		defer first.Release()

		second, err := atomicfile.Acquire(context.Background(), path, false)
		Expect(err).NotTo(HaveOccurred())
		second.Release()
	})

	It("returns on cancellation instead of blocking in flock", func() {
		// The property that matters: a job cancelled while queued behind a
		// long fetch has to be able to unwind.
		path := filepath.Join(GinkgoT().TempDir(), "data.toml")

		held, err := atomicfile.Acquire(context.Background(), path, true)
		Expect(err).NotTo(HaveOccurred())
		defer held.Release()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			_, err := atomicfile.Acquire(ctx, path, false)
			done <- err
		}()
		Eventually(done, "1s").Should(Receive(MatchError(context.DeadlineExceeded)))
	})

	It("admits a waiter once the exclusive holder releases", func() {
		path := filepath.Join(GinkgoT().TempDir(), "data.toml")

		held, err := atomicfile.Acquire(context.Background(), path, true)
		Expect(err).NotTo(HaveOccurred())

		done := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			lock, err := atomicfile.Acquire(context.Background(), path, false)
			Expect(err).NotTo(HaveOccurred())
			lock.Release()
			close(done)
		}()

		Consistently(done, "60ms").ShouldNot(BeClosed())
		held.Release()
		Eventually(done, "1s").Should(BeClosed())
	})
})

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
