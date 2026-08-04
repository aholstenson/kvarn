package orchestrator

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("staging", func() {
	It("admits everything when unbounded", func() {
		for _, g := range []*staging{newStaging(0), newStaging(-1)} {
			Expect(g).To(BeNil())
			release, err := g.acquire(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(g.waiting()).To(BeFalse())
			release()
		}
	})

	It("blocks past its permit count and lets the next in on release", func() {
		g := newStaging(2)

		first, err := g.acquire(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = g.acquire(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(g.waiting()).To(BeTrue())

		got := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			release, err := g.acquire(context.Background())
			Expect(err).NotTo(HaveOccurred())
			defer release()
			close(got)
		}()
		Consistently(got, 50*time.Millisecond).ShouldNot(BeClosed())

		first()
		Eventually(got).Should(BeClosed())
	})

	It("unwinds on a cancelled context rather than waiting for a permit", func() {
		g := newStaging(1)
		release, err := g.acquire(context.Background())
		Expect(err).NotTo(HaveOccurred())
		defer release()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err = g.acquire(ctx)
		Expect(err).To(MatchError(context.DeadlineExceeded))
	})

	It("releases once however many times it is called", func() {
		// The caller hands the permit back as soon as the clone is done and
		// also defers the same func, so a double call must not free a permit
		// that was never taken.
		g := newStaging(1)
		release, err := g.acquire(context.Background())
		Expect(err).NotTo(HaveOccurred())

		release()
		release()
		release()

		second, err := g.acquire(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(g.waiting()).To(BeTrue(), "the extra releases did not invent a permit")
		second()
	})
})
