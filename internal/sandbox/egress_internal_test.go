package sandbox

import (
	"errors"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("recording refused hosts", func() {
	It("keeps first-seen order and ignores repeats", func() {
		s := &Session{}
		s.recordEgressDenied("dl.google.com")
		s.recordEgressDenied("nodejs.org")
		s.recordEgressDenied("dl.google.com")
		Expect(s.DeniedHosts()).To(Equal([]string{"dl.google.com", "nodejs.org"}))
	})

	It("stops recording past the cap", func() {
		// A blocked download inside a retry loop must not grow the list without
		// bound; naming the problem takes a handful of hosts, not a log.
		s := &Session{}
		for i := 0; i < maxRecordedDeniedHosts+10; i++ {
			s.recordEgressDenied(fmt.Sprintf("host-%d.example.com", i))
		}
		Expect(s.DeniedHosts()).To(HaveLen(maxRecordedDeniedHosts))
	})

	It("is safe to call from the proxy's goroutines", func() {
		s := &Session{}
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s.recordEgressDenied(fmt.Sprintf("host-%d.example.com", i%5))
			}(i)
		}
		wg.Wait()
		Expect(s.DeniedHosts()).To(HaveLen(5))
	})
})

var _ = Describe("annotateEgress", func() {
	It("leaves a success alone", func() {
		s := &Session{}
		s.recordEgressDenied("dl.google.com")
		Expect(s.annotateEgress(nil)).To(BeNil())
	})

	It("leaves a failure alone when nothing was refused", func() {
		s := &Session{}
		base := errors.New("exit 1")
		Expect(s.annotateEgress(base)).To(Equal(base))
	})

	It("names the refused hosts and stays unwrappable", func() {
		// The program inside the VM reports "unexpected EOF" because that is
		// all it saw; the host list is the half of the story only the proxy
		// has.
		s := &Session{}
		s.recordEgressDenied("dl.google.com")
		s.recordEgressDenied("release-assets.githubusercontent.com")

		base := errors.New(`provision mise: "mise install" failed`)
		err := s.annotateEgress(base)
		Expect(err).To(MatchError(ContainSubstring("dl.google.com")))
		Expect(err).To(MatchError(ContainSubstring("release-assets.githubusercontent.com")))
		Expect(err).To(MatchError(ContainSubstring("network.allowed_hosts")))
		Expect(errors.Is(err, base)).To(BeTrue())
	})
})
