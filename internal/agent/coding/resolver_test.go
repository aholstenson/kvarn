package coding_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
)

// configWith renders an agents.toml pointing every class at the credential-free
// "test/" provider, so resolution runs against a real store and a real manager
// without reaching for an API key. mainKeys are extra keys for the balanced
// class, which is written last so they land in its block; extra is appended
// after it and must open its own table.
func configWith(mainKeys, extra string) string {
	return `
[models.coding-agent-fast]
model = "test/fast"

[models.coding-agent-reasoning]
model = "test/reasoning"

[models.coding-agent]
model = "test/main"
` + mainKeys + "\n" + extra
}

var _ = Describe("Resolver", func() {
	var (
		ctx      context.Context
		mgr      *llms.Manager
		path     string
		resolver *coding.Resolver
	)

	write := func(contents string) {
		Expect(os.WriteFile(path, []byte(contents), 0o600)).To(Succeed())
	}

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		mgr, err = llms.NewManager()
		Expect(err).NotTo(HaveOccurred())

		path = filepath.Join(GinkgoT().TempDir(), "agents.toml")
		write(configWith("max_steps = 42", ""))
		resolver = coding.NewResolver(mgr, modeltoml.New(path), "")
	})

	It("picks up an edit to max_steps without being recreated", func() {
		models, err := resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(models.ClassConfigs[coding.ModelMain].MaxSteps).To(Equal(42))

		write(configWith("max_steps = 7", ""))

		models, err = resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(models.ClassConfigs[coding.ModelMain].MaxSteps).To(Equal(7))
	})

	It("picks up an edit to a per-agent block without being recreated", func() {
		agentName := slices.Sorted(maps.Keys(coding.DefaultAgentClasses()))[0]

		models, err := resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(models.AgentConfigs).To(HaveKey(agentName))

		write(configWith("max_steps = 42", "[agents."+agentName+"]\nmax_steps = 3\n"))

		models, err = resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(models.AgentConfigs[agentName].MaxSteps).To(Equal(3))
	})

	It("keeps serving the last good configuration when the file stops parsing", func() {
		_, err := resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())

		write("this is not valid toml {{{")

		models, err := resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(models.ClassConfigs[coding.ModelMain].MaxSteps).To(Equal(42))
	})

	It("keeps serving the last good configuration when an alias becomes unknown", func() {
		_, err := resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())

		write(configWith("max_steps = 42", "[models.no-such-class]\nmodel = \"test/x\"\n"))

		models, err := resolver.Resolve(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(models.ClassConfigs[coding.ModelMain].MaxSteps).To(Equal(42))
	})

	It("reports the error when the first resolution fails", func() {
		write("this is not valid toml {{{")

		_, err := coding.NewResolver(mgr, modeltoml.New(path), "").Resolve(ctx)
		Expect(err).To(HaveOccurred())
	})
})
