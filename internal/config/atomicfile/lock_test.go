//go:build unix

package atomicfile_test

import (
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

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
