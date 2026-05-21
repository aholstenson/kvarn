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

func (a *CodingAgent) Run(ctx context.Context, agentCtx *agent.Context) (*agent.Result, error) {
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

	// Collect conversation history for the summary call.
	var conversationHistory []*llms.Message
	conversationHistory = append(conversationHistory, llms.NewMessage(llms.RoleUser, llms.NewTextPart(agentCtx.Prompt)))

	mainCfg := a.configs[ModelMain]
	maxOut := mainCfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}
	maxSteps := mainCfg.MaxSteps
	if maxSteps == 0 {
		maxSteps = 50
	}

	opts := []llms.GenerateOption{
		llms.WithSystemPrompt(systemPrompt),
		llms.WithMessages(conversationHistory...),
		llms.WithMaxSteps(maxSteps),
		llms.WithMaxOutputTokens(maxOut),
	}
	if mode.Tools != nil {
		opts = append(opts, llms.WithTools(mode.Tools(toolkit)...))
	} else {
		opts = append(opts, llms.WithToolkits(toolkit))
	}
	if mainCfg.ThinkingTokens > 0 {
		opts = append(opts, llms.WithMaxThinkingTokens(mainCfg.ThinkingTokens))
	}

	if agentCtx.OnProgress != nil {
		var mu sync.Mutex
		textBufs := make(map[string]*strings.Builder)

		opts = append(opts, llms.WithStreamingFunc(func(ctx context.Context, event llms.StreamingEvent) error {
			var agentID string
			if scope, ok := llms.GetStreamScope(ctx); ok {
				agentID = scope.AgentID
			}
			mu.Lock()
			defer mu.Unlock()
			switch e := event.(type) {
			case llms.StreamingEventTextChunk:
				buf, ok := textBufs[agentID]
				if !ok {
					buf = &strings.Builder{}
					textBufs[agentID] = buf
				}
				buf.WriteString(e.Text)

			case llms.StreamingEventMessageEnd:
				buf := textBufs[agentID]
				if buf != nil && buf.Len() > 0 {
					agentCtx.OnProgress(agent.ProgressTextMessage{
						AgentID: agentID,
						Text:    buf.String(),
						Final:   e.Final,
					})
					buf.Reset()
				}
				if agentCtx.Cost != nil {
					agentCtx.Cost.CheckBudget()
				}

			case llms.StreamingEventToolUse:
				argsJSON, _ := json.Marshal(e.Arguments)
				agentCtx.OnProgress(agent.ProgressToolUse{
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
				agentCtx.OnProgress(agent.ProgressToolResult{
					AgentID: agentID,
					ToolID:  e.ToolID,
					Result:  result,
					IsError: isError,
				})
			}
			return nil
		}))
	}

	if agentCtx.Cost != nil {
		ctx = llms.WithMetrics(ctx, agentCtx.Cost.Recorder())
	}

	finalCost := func() cost.Report {
		if agentCtx.Cost == nil {
			return cost.Report{}
		}
		return agentCtx.Cost.Snapshot()
	}

	mainModel := a.models[ModelMain]
	agenticResult, err := mainModel.GenerateContent(ctx, opts...)
	if agentCtx.Cost != nil {
		agentCtx.Cost.CheckBudget()
	}
	if err != nil {
		return &agent.Result{Cost: finalCost()}, err
	}

	var finalText string
	if textResult, ok := agenticResult.(llms.TextResult); ok {
		finalText = textResult.Text
	}

	// Read-only modes (review, research) produce a written answer as
	// their final message. Return that verbatim — summarizing it into a
	// commit-message-shaped title/description would just discard the
	// content the user actually asked for.
	if !mode.Writes {
		return &agent.Result{
			Description: finalText,
			Cost:        finalCost(),
		}, nil
	}

	conversationHistory = append(conversationHistory, llms.NewMessage(llms.RoleAssistant, llms.NewTextPart(finalText)))

	// The API requires the conversation to end with a user message.
	summaryPrompt := "Summarize the work you just completed for a git commit and pull request.\n\n" +
		"Provide:\n" +
		"- title: an imperative-mood subject line, max 72 chars, no trailing period.\n" +
		"- description: a commit body suitable as both the commit message body and the PR description. Write a few short paragraphs in past tense explaining what changed and why. Wrap lines at ~72 chars. Do not repeat the title. Do not list every file touched. Do not add a footer such as \"Generated by\" or a session id."
	if agentCtx.RepoContext != nil && len(agentCtx.RepoContext.RecentCommits) > 0 {
		var sb strings.Builder
		sb.WriteString(summaryPrompt)
		sb.WriteString("\n\nMatch the commit message style used in this repository. Here are recent commit titles for reference:\n")
		for _, c := range agentCtx.RepoContext.RecentCommits {
			sb.WriteString("- ")
			sb.WriteString(c)
			sb.WriteString("\n")
		}
		summaryPrompt = sb.String()
	}
	conversationHistory = append(conversationHistory, llms.NewMessage(llms.RoleUser, llms.NewTextPart(summaryPrompt)))

	// Build a summary-specific system prompt that includes project
	// instructions (which may contain commit conventions) without the
	// full coding-agent framing.
	var summarySystem strings.Builder
	summarySystem.WriteString("You are a technical writer producing a commit message and pull request description for work that was just completed.")
	if agentCtx.RepoContext != nil && agentCtx.RepoContext.Instructions != "" {
		summarySystem.WriteString("\n\nThe following project instructions may contain commit message conventions you should follow:\n\n")
		summarySystem.WriteString(agentCtx.RepoContext.Instructions)
	}

	// Second call: structured output to get a summary for the commit/PR.
	summaryResult, err := mainModel.GenerateContent(ctx,
		llms.WithSystemPrompt(summarySystem.String()),
		llms.WithMessages(conversationHistory...),
		llms.WithResponseSchema[AgentSummary](),
		llms.WithMaxOutputTokens(1024),
	)
	if agentCtx.Cost != nil {
		agentCtx.Cost.CheckBudget()
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
