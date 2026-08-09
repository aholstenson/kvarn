package model_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// stubStore serves fixed overrides in place of agents.toml. The "test/" model
// provider llms-go recognises needs no credentials, so resolution runs for real.
type stubStore struct {
	models map[string]modelcfg.RawEntry
	agents map[string]modelcfg.RawAgent
	err    error
}

func (s stubStore) All(context.Context) (map[string]modelcfg.RawEntry, error) {
	return s.models, s.err
}

func (s stubStore) Agents(context.Context) (map[string]modelcfg.RawAgent, error) {
	return s.agents, s.err
}

func ptr[T any](v T) *T { return &v }

var _ = Describe("Resolve", func() {
	var (
		ctx      context.Context
		mgr      *llms.Manager
		defaults map[string]modelcfg.Entry
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		mgr, err = llms.NewManager()
		Expect(err).NotTo(HaveOccurred())
		defaults = map[string]modelcfg.Entry{
			"fast":     {ModelID: "test/small", MaxOutputTokens: 8192, MaxSteps: 100},
			"balanced": {ModelID: "test/medium", ReasoningEffort: llms.EffortMedium, MaxOutputTokens: 16384, MaxSteps: 100},
		}
	})

	It("keeps built-in values for keys the override does not mention", func() {
		store := stubStore{models: map[string]modelcfg.RawEntry{
			"fast": {ReasoningEffort: ptr(llms.EffortHigh)},
		}}

		_, cfgs, err := modelcfg.Resolve(ctx, mgr, store, defaults, "balanced", "")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfgs["fast"].ReasoningEffort).To(Equal(llms.EffortHigh))
		Expect(cfgs["fast"].ModelID).To(Equal("test/small"))
		Expect(cfgs["fast"].MaxOutputTokens).To(Equal(8192))
		Expect(cfgs["fast"].MaxSteps).To(Equal(100))
	})

	It("rejects an alias that is not one of the known classes", func() {
		store := stubStore{models: map[string]modelcfg.RawEntry{
			"coding-agent-small": {ModelID: "test/legacy"},
		}}

		_, _, err := modelcfg.Resolve(ctx, mgr, store, defaults, "balanced", "")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`unknown model alias "coding-agent-small"`))
		Expect(err.Error()).To(ContainSubstring("balanced, fast"))
	})

	It("points the main alias at the override the caller supplies", func() {
		models, _, err := modelcfg.Resolve(ctx, mgr, stubStore{}, defaults, "balanced", "test/override")
		Expect(err).NotTo(HaveOccurred())

		direct, err := mgr.GetModel(ctx, "test/override")
		Expect(err).NotTo(HaveOccurred())
		Expect(models["balanced"]).To(BeIdenticalTo(direct))
	})

	It("surfaces a store failure rather than falling back to built-ins", func() {
		store := stubStore{err: errors.New("disk on fire")}

		_, _, err := modelcfg.Resolve(ctx, mgr, store, defaults, "balanced", "")
		Expect(err).To(MatchError(ContainSubstring("disk on fire")))
	})
})

var _ = Describe("ResolveAgents", func() {
	var (
		ctx     context.Context
		mgr     *llms.Manager
		classes map[string]modelcfg.Entry
		agents  map[string]string
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		mgr, err = llms.NewManager()
		Expect(err).NotTo(HaveOccurred())
		classes = map[string]modelcfg.Entry{
			"fast":      {ModelID: "test/small", MaxOutputTokens: 8192, MaxSteps: 100},
			"reasoning": {ModelID: "test/large", ReasoningEffort: llms.EffortHigh, MaxOutputTokens: 16384, MaxSteps: 50},
		}
		agents = map[string]string{"explore": "fast", "plan": "reasoning"}
	})

	It("gives each agent the settings of the class it declares", func() {
		_, cfgs, err := modelcfg.ResolveAgents(ctx, mgr, stubStore{}, classes, agents)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfgs["explore"]).To(Equal(classes["fast"]))
		Expect(cfgs["plan"]).To(Equal(classes["reasoning"]))
	})

	It("moves an agent to another class without touching the class itself", func() {
		store := stubStore{agents: map[string]modelcfg.RawAgent{
			"explore": {Class: "reasoning"},
		}}

		_, cfgs, err := modelcfg.ResolveAgents(ctx, mgr, store, classes, agents)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfgs["explore"]).To(Equal(classes["reasoning"]))
		Expect(classes["fast"].ModelID).To(Equal("test/small"))
	})

	It("layers per-agent keys on top of the class the agent lands on", func() {
		store := stubStore{agents: map[string]modelcfg.RawAgent{
			"explore": {
				Class:    "reasoning",
				RawEntry: modelcfg.RawEntry{MaxSteps: ptr(7)},
			},
		}}

		_, cfgs, err := modelcfg.ResolveAgents(ctx, mgr, store, classes, agents)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfgs["explore"].MaxSteps).To(Equal(7))
		Expect(cfgs["explore"].ModelID).To(Equal("test/large"))
		Expect(cfgs["explore"].ReasoningEffort).To(Equal(llms.EffortHigh))
	})

	It("resolves an agent that names a model no class registers", func() {
		store := stubStore{agents: map[string]modelcfg.RawAgent{
			"plan": {RawEntry: modelcfg.RawEntry{ModelID: "test/bespoke"}},
		}}

		models, cfgs, err := modelcfg.ResolveAgents(ctx, mgr, store, classes, agents)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfgs["plan"].ModelID).To(Equal("test/bespoke"))
		Expect(models["plan"]).NotTo(BeNil())
	})

	It("rejects an override for an agent that does not exist", func() {
		store := stubStore{agents: map[string]modelcfg.RawAgent{
			"reviewer": {Class: "fast"},
		}}

		_, _, err := modelcfg.ResolveAgents(ctx, mgr, store, classes, agents)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`unknown agent "reviewer"`))
		Expect(err.Error()).To(ContainSubstring("explore, plan"))
	})

	It("rejects an override that names a class that does not exist", func() {
		store := stubStore{agents: map[string]modelcfg.RawAgent{
			"plan": {Class: "genius"},
		}}

		_, _, err := modelcfg.ResolveAgents(ctx, mgr, store, classes, agents)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`unknown class "genius"`))
		Expect(err.Error()).To(ContainSubstring("fast, reasoning"))
	})
})
