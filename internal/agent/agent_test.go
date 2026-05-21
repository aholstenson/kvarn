package agent_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent"
)

var _ = Describe("NoopAgent", func() {
	It("runs without error", func() {
		a := &agent.NoopAgent{}
		result, err := a.Run(context.Background(), &agent.Context{
			ProjectName: "test",
			Prompt:      "do something",
			WorkingDir:  "/home/kvarn/workspace",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
	})
})
