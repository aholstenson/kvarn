package dispatch_test

import (
	"fmt"
	"io"
	"strings"
	"sync"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/dispatch"
)

var _ = Describe("Registry", func() {
	var r *dispatch.Registry

	BeforeEach(func() {
		r = dispatch.NewRegistry()
	})

	Describe("Register and Lookup", func() {
		It("returns a PendingRunner that can be looked up", func() {
			pr, err := r.Register("token-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(pr).NotTo(BeNil())

			found, ok := r.Lookup("token-1")
			Expect(ok).To(BeTrue())
			Expect(found).To(Equal(pr))
		})
	})

	Describe("Duplicate rejection", func() {
		It("rejects registering the same token twice", func() {
			_, err := r.Register("token-1")
			Expect(err).NotTo(HaveOccurred())

			_, err = r.Register("token-1")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Unknown token", func() {
		It("returns false for an unregistered token", func() {
			_, ok := r.Lookup("nonexistent")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("Remove", func() {
		It("removes a previously registered token", func() {
			_, err := r.Register("token-1")
			Expect(err).NotTo(HaveOccurred())

			r.Remove("token-1")

			_, ok := r.Lookup("token-1")
			Expect(ok).To(BeFalse())
		})
	})
})

var _ = Describe("PendingRunner transfers", func() {
	var pr *dispatch.PendingRunner

	BeforeEach(func() {
		pr = &dispatch.PendingRunner{
			CommandCh: make(chan *v1.RunnerCommand, 1),
			ResultCh:  make(chan *v1.CommandResult, 1),
			OutputCh:  make(chan *v1.OutputChunk, 64),
			DoneCh:    make(chan struct{}),
		}
	})

	Describe("RegisterTransfer", func() {
		It("registers a transfer that can be looked up", func() {
			t := &dispatch.PendingTransfer{
				Reader: io.NopCloser(strings.NewReader("data")),
				Meta:   &v1.FileStreamStart{TransferId: "t-1"},
				Done:   make(chan struct{}),
			}
			pr.RegisterTransfer("t-1", t)

			found, ok := pr.LookupTransfer("t-1")
			Expect(ok).To(BeTrue())
			Expect(found).To(Equal(t))
		})
	})

	Describe("LookupTransfer", func() {
		It("returns nil/false for unknown transfer IDs", func() {
			_, ok := pr.LookupTransfer("nonexistent")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("RemoveTransfer", func() {
		It("removes a transfer so subsequent lookup returns nil", func() {
			t := &dispatch.PendingTransfer{
				Reader: io.NopCloser(strings.NewReader("data")),
				Meta:   &v1.FileStreamStart{TransferId: "t-1"},
				Done:   make(chan struct{}),
			}
			pr.RegisterTransfer("t-1", t)
			pr.RemoveTransfer("t-1")

			_, ok := pr.LookupTransfer("t-1")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("Concurrent access", func() {
		It("is safe under parallel register/lookup/remove", func() {
			var wg sync.WaitGroup
			for i := range 100 {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					t := &dispatch.PendingTransfer{
						Reader: io.NopCloser(strings.NewReader("data")),
						Meta:   &v1.FileStreamStart{TransferId: id},
						Done:   make(chan struct{}),
					}
					pr.RegisterTransfer(id, t)
					pr.LookupTransfer(id)
					pr.RemoveTransfer(id)
				}(fmt.Sprintf("t-%d", i))
			}
			wg.Wait()
		})
	})
})
