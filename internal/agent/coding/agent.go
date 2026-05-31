package coding

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// AgentSummary is the structured output format for the summary call.
type AgentSummary struct {
	Title       string `json:"title"       jsonschema:"description=Imperative-mood summary of the change, max 72 chars"`
	Description string `json:"description" jsonschema:"description=Commit body: a few short paragraphs in past tense describing what changed and why. Wrap lines at ~72 chars. No bullet-list of files. No trailing footer."`
}

// CodingAgent is an LLM-powered agent that can modify files in a VM.
type CodingAgent struct {
	models  map[string]llms.Model
	configs map[string]modelcfg.Entry
}

// NewCodingAgent creates a new coding agent. The models map must contain at
// least ModelMain; sub-agents that declare a different alias (e.g. ModelSmall
// for Explore) require the corresponding entry to also be present. configs
// carries the resolved per-alias settings (thinking budget, max output tokens).
func NewCodingAgent(models map[string]llms.Model, configs map[string]modelcfg.Entry) *CodingAgent {
	return &CodingAgent{models: models, configs: configs}
}

// Start opens a stateful llms.Session so the orchestrator can drive multiple
// agent turns through the same conversation. The session preserves message
// history (tool calls and results) across turns, which is what makes
// validation-failure retries productive — the agent sees what it tried last
// time.
func (a *CodingAgent) Start(ctx context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	var skills []repocontext.Skill
	if agentCtx.RepoContext != nil {
		skills = agentCtx.RepoContext.Skills
	}

	mode := ModeAuto
	if agentCtx.Mode != nil {
		if m, ok := agentCtx.Mode.(*Mode); ok {
			mode = m
		}
	}

	subAgents := SubAgents{
		Explore.Name: Explore,
		Plan.Name:    Plan,
	}

	toolkit := NewCodingToolkitWithOpts(CodingToolkitOpts{
		Runner:     agentCtx.Runner,
		WorkingDir: agentCtx.WorkingDir,
		SessionID:  agentCtx.SessionID,
		Skills:     skills,
		Models:     a.models,
		Configs:    a.configs,
		SubAgents:  subAgents,
		RepoCtx:    agentCtx.RepoContext,
		Tracker:    agentCtx.Cost,
	})
	systemPrompt := mode.SystemPrompt(agentCtx.ProjectName, agentCtx.RepoURL, agentCtx.Branch, agentCtx.RepoContext, subAgents)

	mainCfg := a.configs[ModelMain]
	maxOut := mainCfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}
	maxSteps := mainCfg.MaxSteps
	if maxSteps == 0 {
		maxSteps = 50
	}

	c := &codingConversation{
		agent:    a,
		agentCtx: agentCtx,
		mode:     mode,
		mainCfg:  mainCfg,
		textBufs: make(map[string]*strings.Builder),
	}

	opts := []llms.GenerateOption{
		llms.WithSystemPrompt(systemPrompt),
		llms.WithMessages(llms.NewMessage(llms.RoleUser, llms.NewTextPart(agentCtx.Prompt))),
		llms.WithMaxSteps(maxSteps),
		llms.WithMaxOutputTokens(maxOut),
	}
	if mode.Tools != nil {
		opts = append(opts, llms.WithTools(mode.Tools(toolkit)...))
	} else {
		opts = append(opts, llms.WithToolkits(toolkit))
	}
	if mainCfg.ReasoningEffort != "" {
		opts = append(opts, llms.WithReasoningEffort(mainCfg.ReasoningEffort))
	}

	if agentCtx.OnProgress != nil {
		opts = append(opts, llms.WithStreamingFunc(c.handleStreamingEvent))
	}

	mainModel := a.models[ModelMain]
	sessCtx := ctx
	if agentCtx.Cost != nil {
		sessCtx = llms.WithMetrics(sessCtx, agentCtx.Cost.Recorder())
	}
	sess, err := llms.NewSession(sessCtx, mainModel, opts...)
	if err != nil {
		return nil, err
	}
	c.sess = sess

	return c, nil
}

// codingConversation drives a single llms.Session across one or more agent
// turns. The streaming-event handler reuses the same per-agent text buffers
// across calls so partial-message text never leaks between turns.
type codingConversation struct {
	agent    *CodingAgent
	agentCtx *agent.Context
	mode     *Mode
	mainCfg  modelcfg.Entry
	sess     *llms.Session

	streamMu sync.Mutex
	textBufs map[string]*strings.Builder
}

// Run advances the session to its next stopping point: either the assistant
// finishes its turn with a final text reply, or the step budget is exhausted.
// On the first call followup must be empty (the constructor's prompt is
// already in the message history); on later calls followup is injected as the
// next user message.
func (c *codingConversation) Run(ctx context.Context, followup string) (string, error) {
	if followup != "" {
		c.sess.Inject(llms.NewMessage(llms.RoleUser, llms.NewTextPart(followup)))
	}

	if c.agentCtx.Cost != nil {
		ctx = llms.WithMetrics(ctx, c.agentCtx.Cost.Recorder())
	}

	for {
		_, done, err := c.sess.Step(ctx)
		if err != nil {
			if c.agentCtx.Cost != nil {
				c.agentCtx.Cost.CheckBudget()
			}
			return "", err
		}
		if done {
			break
		}
	}
	if c.agentCtx.Cost != nil {
		c.agentCtx.Cost.CheckBudget()
	}

	result, err := c.sess.Result()
	if err != nil {
		return "", err
	}
	if textResult, ok := result.(llms.TextResult); ok {
		return textResult.Text, nil
	}
	return "", nil
}

// Summarize asks the model to produce a commit title and PR body for the work
// already in the session's message history. Read-only modes don't go through
// here — the orchestrator returns the final Run text directly.
func (c *codingConversation) Summarize(ctx context.Context) (*agent.Result, error) {
	if c.agentCtx.Cost != nil {
		ctx = llms.WithMetrics(ctx, c.agentCtx.Cost.Recorder())
	}

	finalCost := func() cost.Report {
		if c.agentCtx.Cost == nil {
			return cost.Report{}
		}
		return c.agentCtx.Cost.Snapshot()
	}

	// The summary uses the accumulated message history plus a trailing user
	// turn that requests the commit/PR summary. We do not feed the session's
	// tool list here — a one-shot structured-output call has no need for
	// tools, and the API requires the transcript to end with a user message.
	summaryPrompt := "Summarize the work you just completed for a git commit and pull request.\n\n" +
		"Provide:\n" +
		"- title: an imperative-mood subject line, max 72 chars, no trailing period.\n" +
		"- description: a commit body suitable as both the commit message body and the PR description. Write a few short paragraphs in past tense explaining what changed and why. Wrap lines at ~72 chars. Do not repeat the title. Do not list every file touched. Do not add a footer such as \"Generated by\" or a session id."
	if c.agentCtx.RepoContext != nil && len(c.agentCtx.RepoContext.RecentCommits) > 0 {
		var sb strings.Builder
		sb.WriteString(summaryPrompt)
		sb.WriteString("\n\nMatch the commit message style used in this repository. Here are recent commit titles for reference:\n")
		for _, commit := range c.agentCtx.RepoContext.RecentCommits {
			sb.WriteString("- ")
			sb.WriteString(commit)
			sb.WriteString("\n")
		}
		summaryPrompt = sb.String()
	}

	messages := append(c.sess.Messages(), llms.NewMessage(llms.RoleUser, llms.NewTextPart(summaryPrompt)))

	var summarySystem strings.Builder
	summarySystem.WriteString("You are a technical writer producing a commit message and pull request description for work that was just completed.")
	if c.agentCtx.RepoContext != nil && c.agentCtx.RepoContext.Instructions != "" {
		summarySystem.WriteString("\n\nThe following project instructions may contain commit message conventions you should follow:\n\n")
		summarySystem.WriteString(c.agentCtx.RepoContext.Instructions)
	}

	mainModel := c.agent.models[ModelMain]
	summaryResult, err := mainModel.GenerateContent(ctx,
		llms.WithSystemPrompt(summarySystem.String()),
		llms.WithMessages(messages...),
		llms.WithResponseSchema[AgentSummary](),
		llms.WithMaxOutputTokens(1024),
	)
	if c.agentCtx.Cost != nil {
		c.agentCtx.Cost.CheckBudget()
	}
	if err != nil {
		slog.Warn("failed to generate summary, using default", "error", err)
		return &agent.Result{
			Title:       "Apply agent changes",
			Description: "Automated changes by kvarn agent.",
			Cost:        finalCost(),
		}, nil
	}

	if structured, ok := summaryResult.(llms.StructuredResult[AgentSummary]); ok {
		return &agent.Result{
			Title:       structured.Data.Title,
			Description: structured.Data.Description,
			Cost:        finalCost(),
		}, nil
	}

	return &agent.Result{
		Title:       "Apply agent changes",
		Description: "Automated changes by kvarn agent.",
		Cost:        finalCost(),
	}, nil
}

func (c *codingConversation) Close() error {
	return nil
}

// handleStreamingEvent fans an llms streaming event out to the agent.Context
// progress callback. The per-agent text buffer is kept on the receiver so a
// long assistant message split across many TextChunk events still arrives at
// the orchestrator as a single ProgressTextMessage at MessageEnd.
func (c *codingConversation) handleStreamingEvent(ctx context.Context, event llms.StreamingEvent) error {
	var agentID string
	if scope, ok := llms.GetStreamScope(ctx); ok {
		agentID = scope.AgentID
	}
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	switch e := event.(type) {
	case llms.StreamingEventTextChunk:
		buf, ok := c.textBufs[agentID]
		if !ok {
			buf = &strings.Builder{}
			c.textBufs[agentID] = buf
		}
		buf.WriteString(e.Text)

	case llms.StreamingEventMessageEnd:
		buf := c.textBufs[agentID]
		if buf != nil && buf.Len() > 0 {
			c.agentCtx.OnProgress(agent.ProgressTextMessage{
				AgentID: agentID,
				Text:    buf.String(),
				Final:   e.Final,
			})
			buf.Reset()
		}
		if c.agentCtx.Cost != nil {
			c.agentCtx.Cost.CheckBudget()
		}

	case llms.StreamingEventToolUse:
		argsJSON, _ := json.Marshal(e.Arguments)
		c.agentCtx.OnProgress(agent.ProgressToolUse{
			AgentID:       agentID,
			ToolID:        e.ToolID,
			ArgumentsJSON: string(argsJSON),
		})

	case llms.StreamingEventToolResult:
		result := ""
		isError := false
		if e.Result != nil {
			if err, ok := e.Result.(error); ok {
				result = err.Error()
				isError = true
			} else if s, ok := e.Result.(string); ok {
				result = s
			} else {
				b, _ := json.Marshal(e.Result)
				result = string(b)
			}
		}
		c.agentCtx.OnProgress(agent.ProgressToolResult{
			AgentID: agentID,
			ToolID:  e.ToolID,
			Result:  result,
			IsError: isError,
		})

	case llms.StreamingEventToolError:
		// Terminal event emitted in place of StreamingEventToolResult when a
		// tool call fails; surface it so the call leaves the running state
		// instead of being pinned there.
		result := ""
		if e.Error != nil {
			result = e.Error.Error()
		}
		c.agentCtx.OnProgress(agent.ProgressToolResult{
			AgentID: agentID,
			ToolID:  e.ToolID,
			Result:  result,
			IsError: true,
		})
	}
	return nil
}

