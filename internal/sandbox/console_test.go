package sandbox_test

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/sandbox"
)

var _ = Describe("ConsoleTail", func() {
	It("is empty before the guest says anything", func() {
		Expect(sandbox.NewConsoleTail().String()).To(BeEmpty())
	})

	It("keeps lines in the order the guest printed them", func() {
		tail := sandbox.NewConsoleTail()
		tail.Add("booting\n")
		tail.Add("ready\n")

		Expect(tail.Lines()).To(Equal([]string{"booting", "ready"}))
		Expect(tail.String()).To(Equal("booting\nready"))
	})

	It("splits a chunk carrying several lines", func() {
		tail := sandbox.NewConsoleTail()
		tail.Add("first\nsecond\nthird\n")

		Expect(tail.Lines()).To(Equal([]string{"first", "second", "third"}))
	})

	It("drops blank lines so they do not crowd out the tail", func() {
		tail := sandbox.NewConsoleTail()
		tail.Add("first\n")
		tail.Add("\n")
		tail.Add("   \n")
		tail.Add("second\n")

		Expect(tail.Lines()).To(Equal([]string{"first", "second"}))
	})

	It("keeps the end of a long console rather than the start", func() {
		tail := sandbox.NewConsoleTail()
		for i := range 200 {
			tail.Add(fmt.Sprintf("line %d\n", i))
		}

		lines := tail.Lines()
		Expect(len(lines)).To(BeNumerically("<=", 50))
		Expect(lines[len(lines)-1]).To(Equal("line 199"))
		Expect(strings.Join(lines, "\n")).NotTo(ContainSubstring("line 0\n"))
	})

	It("tolerates a nil tail so a proxy without a console still reports", func() {
		var tail *sandbox.ConsoleTail
		Expect(func() { tail.Add("anything") }).NotTo(Panic())
		Expect(tail.String()).To(BeEmpty())
	})
})
