package coding_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent/coding"
)

// fakeModel is a minimal llms.Model implementation for testing spawn_agent.
type fakeModel struct {
	generate func(ctx context.Context, opts ...llms.GenerateOption) (llms.Result, error)
}

func (f *fakeModel) GenerateContent(ctx context.Context, opts ...llms.GenerateOption) (llms.Result, error) {
	if f.generate != nil {
		return f.generate(ctx, opts...)
	}
	return llms.TextResult{Text: "ok"}, nil
}

// sessionTrackingRunner is a mockRunner that records CreateSession/CloseSession calls.
type sessionTrackingRunner struct {
	mockRunner
	createCalls []*v1.CreateSessionRequest
	closeCalls  []*v1.CloseSessionRequest
	nextSession string
}

func (s *sessionTrackingRunner) CreateSession(_ context.Context, req *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	s.createCalls = append(s.createCalls, req)
	id := s.nextSession
	if id == "" {
		id = "sub-session"
	}
	return &v1.CreateSessionResponse{SessionId: id}, nil
}

func (s *sessionTrackingRunner) CloseSession(_ context.Context, req *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	s.closeCalls = append(s.closeCalls, req)
	return &v1.CloseSessionResponse{}, nil
}

var _ = Describe("spawn_agent", func() {
	var (
		runner *sessionTrackingRunner
		ctx    context.Context
	)

	BeforeEach(func() {
		runner = &sessionTrackingRunner{}
		ctx = context.Background()
	})

	buildToolkit := func(model llms.Model, subs coding.SubAgents) *coding.CodingToolkit {
		return coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
			Runner:     runner,
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "parent-session",
			Models: map[string]llms.Model{
				coding.ModelMain:  model,
				coding.ModelSmall: model,
			},
			SubAgents: subs,
		})
	}

	findSpawnTool := func(tk *coding.CodingToolkit) llms.ToolDef {
		for _, t := range tk.Tools() {
			if t.Name() == "spawn_agent" {
				return t
			}
		}
		return nil
	}

	It("is not registered when no sub-agents are configured", func() {
		tk := coding.NewCodingToolkit(runner, "/home/kvarn/workspace", "sess", nil)
		Expect(findSpawnTool(tk)).To(BeNil())
	})

	It("is not registered when no models are configured", func() {
		tk := coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
			Runner:     runner,
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "sess",
			SubAgents:  coding.SubAgents{coding.Explore.Name: coding.Explore},
		})
		Expect(findSpawnTool(tk)).To(BeNil())
	})

	It("returns an error for an unknown sub-agent name without creating a session", func() {
		tk := buildToolkit(&fakeModel{}, coding.SubAgents{coding.Explore.Name: coding.Explore})
		tool := findSpawnTool(tk)
		Expect(tool).NotTo(BeNil())

		_, err := tool.Execute(ctx, &coding.SpawnAgentInput{Name: "ghost", Prompt: "go"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown sub-agent"))
		Expect(runner.createCalls).To(BeEmpty())
		Expect(runner.closeCalls).To(BeEmpty())
	})

	It("creates a session, invokes the model, and returns the text", func() {
		called := false
		var capturedScope llms.StreamScope
		var capturedHasScope bool
		model := &fakeModel{
			generate: func(ctx context.Context, _ ...llms.GenerateOption) (llms.Result, error) {
				called = true
				capturedScope, capturedHasScope = llms.GetStreamScope(ctx)
				return llms.TextResult{Text: "explored"}, nil
			},
		}
		runner.nextSession = "fresh-sub"

		tk := buildToolkit(model, coding.SubAgents{coding.Explore.Name: coding.Explore})
		tool := findSpawnTool(tk)

		result, err := tool.Execute(ctx, &coding.SpawnAgentInput{
			Name:   coding.Explore.Name,
			Prompt: "find the entry point",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(called).To(BeTrue())

		Expect(runner.createCalls).To(HaveLen(1))
		Expect(runner.createCalls[0].WorkingDir).To(Equal("/home/kvarn/workspace"))
		Expect(runner.closeCalls).To(HaveLen(1))
		Expect(runner.closeCalls[0].SessionId).To(Equal("fresh-sub"))

		Expect(capturedHasScope).To(BeTrue())
		Expect(capturedScope.AgentID).To(HavePrefix(coding.Explore.Name + "/"))
		Expect(capturedScope.RunID).NotTo(BeEmpty())

		out := result.(*coding.SpawnAgentOutput)
		Expect(out.Text).To(Equal("explored"))
	})

	It("routes each sub-agent to the model alias it declares", func() {
		var mainCalls, smallCalls int
		mainModel := &fakeModel{
			generate: func(_ context.Context, _ ...llms.GenerateOption) (llms.Result, error) {
				mainCalls++
				return llms.TextResult{Text: "main"}, nil
			},
		}
		smallModel := &fakeModel{
			generate: func(_ context.Context, _ ...llms.GenerateOption) (llms.Result, error) {
				smallCalls++
				return llms.TextResult{Text: "small"}, nil
			},
		}

		tk := coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
			Runner:     runner,
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "parent-session",
			Models: map[string]llms.Model{
				coding.ModelMain:  mainModel,
				coding.ModelSmall: smallModel,
			},
			SubAgents: coding.SubAgents{
				coding.Explore.Name: coding.Explore,
				coding.Plan.Name:    coding.Plan,
			},
		})
		tool := findSpawnTool(tk)
		Expect(tool).NotTo(BeNil())

		_, err := tool.Execute(ctx, &coding.SpawnAgentInput{Name: coding.Explore.Name, Prompt: "explore"})
		Expect(err).NotTo(HaveOccurred())
		_, err = tool.Execute(ctx, &coding.SpawnAgentInput{Name: coding.Plan.Name, Prompt: "plan"})
		Expect(err).NotTo(HaveOccurred())

		Expect(smallCalls).To(Equal(1))
		Expect(mainCalls).To(Equal(1))
	})

	It("errors when the sub-agent's model alias is not configured", func() {
		tk := coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
			Runner:     runner,
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "parent-session",
			Models: map[string]llms.Model{
				coding.ModelMain: &fakeModel{},
				// ModelSmall intentionally missing.
			},
			SubAgents: coding.SubAgents{coding.Explore.Name: coding.Explore},
		})
		tool := findSpawnTool(tk)
		Expect(tool).NotTo(BeNil())

		_, err := tool.Execute(ctx, &coding.SpawnAgentInput{Name: coding.Explore.Name, Prompt: "explore"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(coding.ModelSmall))
		Expect(runner.createCalls).To(BeEmpty())
	})

	It("closes the session even when the model returns an error", func() {
		model := &fakeModel{
			generate: func(_ context.Context, _ ...llms.GenerateOption) (llms.Result, error) {
				return nil, errors.New("model blew up")
			},
		}
		tk := buildToolkit(model, coding.SubAgents{coding.Plan.Name: coding.Plan})
		tool := findSpawnTool(tk)

		_, err := tool.Execute(ctx, &coding.SpawnAgentInput{
			Name:   coding.Plan.Name,
			Prompt: "plan it",
		})
		Expect(err).To(HaveOccurred())
		Expect(runner.createCalls).To(HaveLen(1))
		Expect(runner.closeCalls).To(HaveLen(1))
	})
})

var _ = Describe("Mode", func() {
	It("ModeImplement reports its name", func() {
		Expect(coding.ModeImplement.ModeName()).To(Equal("implement"))
	})

	It("includes sub-agent list in the system prompt when sub-agents are provided", func() {
		subs := coding.SubAgents{
			coding.Explore.Name: coding.Explore,
			coding.Plan.Name:    coding.Plan,
		}
		prompt := coding.ModeImplement.SystemPrompt("proj", "url", "main", nil, subs)
		Expect(prompt).To(ContainSubstring("<available_sub_agents>"))
		Expect(prompt).To(ContainSubstring(coding.Explore.Name))
		Expect(prompt).To(ContainSubstring(coding.Plan.Name))
	})

	It("omits sub-agent section when no sub-agents are provided", func() {
		prompt := coding.ModeImplement.SystemPrompt("proj", "url", "main", nil, nil)
		Expect(prompt).NotTo(ContainSubstring("<available_sub_agents>"))
	})
})

var _ = Describe("SubAgent definitions", func() {
	It("Explore exposes read-only tools", func() {
		deps := coding.SubAgentDeps{
			Runner:     &mockRunner{},
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "sub-sess",
		}
		names := map[string]bool{}
		for _, t := range coding.Explore.Tools(deps) {
			names[t.Name()] = true
		}
		Expect(names).To(HaveKey("read_file"))
		Expect(names).To(HaveKey("list_files"))
		Expect(names).To(HaveKey("search_files"))
		Expect(names).NotTo(HaveKey("exec_command"))
		Expect(names).NotTo(HaveKey("edit_file"))
		Expect(names).NotTo(HaveKey("write_file"))
		Expect(names).NotTo(HaveKey("spawn_agent"))
	})

	It("Plan exposes read-only tools", func() {
		deps := coding.SubAgentDeps{
			Runner:     &mockRunner{},
			WorkingDir: "/home/kvarn/workspace",
			SessionID:  "sub-sess",
		}
		names := map[string]bool{}
		for _, t := range coding.Plan.Tools(deps) {
			names[t.Name()] = true
		}
		Expect(names).To(HaveKey("read_file"))
		Expect(names).To(HaveKey("list_files"))
		Expect(names).To(HaveKey("search_files"))
		Expect(names).NotTo(HaveKey("write_file"))
	})
})
