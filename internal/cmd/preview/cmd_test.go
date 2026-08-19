package preview

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var _ = Describe("formatAge", func() {
	It("reads an unset timestamp as a dash", func() {
		// A stopped or failed preview has no start time, which reaches the CLI
		// as a nil timestamp rather than as an epoch instant.
		Expect(formatAge(nil)).To(Equal("-"))
		Expect(formatAge(timestamppb.New(time.Time{}))).To(Equal("-"))
		Expect(formatAge(timestamppb.New(time.Unix(0, 0)))).To(Equal("-"))
	})

	It("renders a recent timestamp as a compact age", func() {
		Expect(formatAge(timestamppb.New(time.Now().Add(-30 * time.Second)))).To(Equal("30s"))
		Expect(formatAge(timestamppb.New(time.Now().Add(-90 * time.Minute)))).To(Equal("1h30m"))
		Expect(formatAge(timestamppb.New(time.Now().Add(-50 * time.Hour)))).To(Equal("2d2h"))
	})
})
