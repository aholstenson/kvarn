package agent_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent"
)

var _ = Describe("NoopAgent", func() {
	It("runs without error via RunOnce", func() {
		a := &agent.NoopAgent{}
		result, err := agent.RunOnce(context.Background(), a, &agent.Context{
			ProjectName: "test",
			Prompt:      "do something",
			WorkingDir:  "/home/kvarn/workspace",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())
	})
})
