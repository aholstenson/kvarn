package coding

import (
	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// SubAgent is a named, specialized agent the parent coding agent can spawn via
// the spawn_agent tool. Each sub-agent runs an independent LLM loop with its
// own system prompt, toolset, and shell session, and returns only its final
// text to the parent.
type SubAgent struct {
	// Name identifies the sub-agent in the spawn_agent tool call (e.g. "explore").
	Name string
	// Description is shown to the parent so it knows when to spawn this agent.
	Description string
	// SystemPrompt builds the system prompt for a sub-agent run.
	SystemPrompt func(rc *repocontext.RepoContext) string
	// Tools constructs the sub-agent's toolkit from the run-scoped dependencies.
	Tools func(deps SubAgentDeps) []llms.ToolDef
	// Class is the capability tier the sub-agent runs on. The model, reasoning
	// effort, output cap and step budget all come from the resolved class, so
	// the definition states what kind of work this is and leaves how much model
	// to spend on it to configuration.
	Class Class
	// Edits marks a sub-agent whose job is to change files. A read-only run
	// does not offer it, because without edit tools it could do nothing its
	// description promises.
	Edits bool
}

// BuiltinSubAgents returns the sub-agents every coding-agent run registers.
func BuiltinSubAgents() SubAgents {
	return SubAgents{
		Explore.Name: Explore,
		Plan.Name:    Plan,
		General.Name: General,
		Worker.Name:  Worker,
	}
}

// ReadOnly returns the sub-agents a read-only run may offer: every one that
// is not there to change files.
func (s SubAgents) ReadOnly() SubAgents {
	out := make(SubAgents, len(s))
	for name, sub := range s {
		if !sub.Edits {
			out[name] = sub
		}
	}
	return out
}

// SubAgentDeps carries everything a sub-agent's toolkit needs to construct
// itself for a single run.
type SubAgentDeps struct {
	Runner     sandbox.RunnerProxy
	WorkingDir string
	SessionID  string
	Skills     map[string]*repocontext.Skill
	// ReadOnly is set when the parent runs without edit tools. A sub-agent
	// that can write must then leave its own edit tools out, so that spawning
	// it cannot change files the parent itself may not change.
	ReadOnly bool
	// Model is the model the sub-agent runs on, which view_file checks a
	// file against.
	Model llms.Model
}

// SubAgents holds the registered sub-agent types keyed by Name.
type SubAgents map[string]*SubAgent

// fullTools returns the tools shared by General and Worker: the parent's own
// tools without spawn_agent and the merge tools, and without the edit tools in
// a read-only run.
func fullTools(deps SubAgentDeps) []llms.ToolDef {
	tk := &CodingToolkit{
		runner:     deps.Runner,
		workingDir: deps.WorkingDir,
		sessionID:  deps.SessionID,
		skills:     deps.Skills,
		tasks:      NewTaskList(),
		viewModel:  deps.Model,
	}
	// The toolkit has no sub-agents and no merge target, so neither
	// spawn_agent nor the merge tools are in the set it returns.
	if deps.ReadOnly {
		return tk.ReadOnlyTools()
	}
	return tk.Tools()
}

// readOnlyTools returns the read-only tools shared by Explore and Plan.
func readOnlyTools(deps SubAgentDeps) []llms.ToolDef {
	tk := &CodingToolkit{
		runner:     deps.Runner,
		workingDir: deps.WorkingDir,
		sessionID:  deps.SessionID,
		skills:     deps.Skills,
		tasks:      NewTaskList(),
		viewModel:  deps.Model,
	}
	// Guarded like the parent's toolkit: a sub-agent has its own conversation
	// to fill up, and an unbounded result ends its run just as surely.
	return tk.guard([]llms.ToolDef{
		llms.NewToolDef(&readFileTool{toolkit: tk}),
		llms.NewToolDef(&viewFileTool{toolkit: tk}),
		llms.NewToolDef(&listFilesTool{toolkit: tk}),
		llms.NewToolDef(&searchFilesTool{toolkit: tk}),
	})
}
