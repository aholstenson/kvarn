package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// summaryMaxOutputTokens bounds the summary call. It is well above what a
// title and a few paragraphs need because a repository can request sections,
// and a body truncated mid-section would land on the pull request that way.
// The call also reasons before it answers, and that budget is spent from the
// same allowance, so the headroom covers both.
const summaryMaxOutputTokens = 16384

// AgentSummary is the structured output format for the summary call.
//
// Field order is load-bearing. The schema is generated from this struct with
// its fields in declaration order, and constrained JSON generation fills the
// properties in schema order and cannot revise what it has already emitted.
// The title therefore comes last: naming a change is easier once the change has
// been described, and a title generated first — before a single word of the
// body — is where filler like "placeholder" comes from.
//
// Sections is a list rather than a field per declared section because the
// schema is derived from this type at compile time: what a repository asks for
// is not known until its kvarn.yml is read. The request names the sections and
// renderBody puts them back in declared order, so the ordering the model
// happens to return them in does not matter.
//
// No field carries `omitempty`: OpenAI's strict structured output rejects a
// schema whose `required` does not list every property, and the reflector
// derives `required` from exactly those fields that lack the option. A request
// naming no sections gets an empty array rather than an absent key.
//
// Descriptions must not contain a comma. The `jsonschema` tag is a
// comma-separated option list, so a comma silently truncates the description at
// that point and the rest never reaches the model.
type AgentSummary struct {
	Description string           `json:"description" jsonschema:"description=Commit body: a few short paragraphs in past tense describing what changed and why. Wrap lines at ~72 chars. No bullet-list of files. No trailing footer."`
	Sections    []SummarySection `json:"sections"    jsonschema:"description=One entry per section named in the request. Empty when the request names none."`
	Title       string           `json:"title"       jsonschema:"description=Imperative-mood summary of the change just described. Stay within the character budget stated in the request."`
}

// SummarySection is one filled-in section of the body.
type SummarySection struct {
	Name    string `json:"name"    jsonschema:"description=The section name exactly as given in the request"`
	Content string `json:"content" jsonschema:"description=Markdown content for this section. No heading line; the heading is added when the body is assembled."`
}

// CodingAgent is an LLM-powered agent that can modify files in a VM.
type CodingAgent struct {
	resolver *Resolver
}

// NewCodingAgent creates a new coding agent that draws its model set from
// resolver. The set is resolved once per conversation rather than held for the
// lifetime of the agent, so configuration edits apply to the next job.
func NewCodingAgent(resolver *Resolver) *CodingAgent {
	return &CodingAgent{resolver: resolver}
}

// Start opens a stateful llms.Session so the orchestrator can drive multiple
// agent turns through the same conversation. The session preserves message
// history (tool calls and results) across turns, which is what makes
// validation-failure retries productive — the agent sees what it tried last
// time.
func (a *CodingAgent) Start(ctx context.Context, agentCtx *agent.Context) (agent.Conversation, error) {
	// Resolved once here and then carried by the conversation: every turn of a
	// job, including the closing summary call, runs on the configuration the
	// job started with, so an edit landing mid-job cannot change the model or
	// the step budget out from under a session already in progress.
	models, err := a.resolver.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve agent models: %w", err)
	}

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

	subAgents := BuiltinSubAgents()

	toolkit := NewCodingToolkitWithOpts(CodingToolkitOpts{
		Runner:       agentCtx.Runner,
		WorkingDir:   agentCtx.WorkingDir,
		SessionID:    agentCtx.SessionID,
		Skills:       skills,
		AgentModels:  models.Agents,
		AgentConfigs: models.AgentConfigs,
		SubAgents:    subAgents,
		RepoCtx:      agentCtx.RepoContext,
		Tracker:      agentCtx.Cost,
	})
	systemPrompt := mode.SystemPrompt(agentCtx.ProjectName, agentCtx.RepoURL, agentCtx.Branch, agentCtx.RepoContext, subAgents, agentCtx.PullRequest)

	mainCfg := models.ClassConfigs[ModelMain]
	maxOut := mainCfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}
	maxSteps := mainCfg.MaxSteps
	if maxSteps == 0 {
		maxSteps = 50
	}

	c := &codingConversation{
		models:   models,
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
		llms.WithToolCallTimeout(hostToolCallTimeout),
	}
	if mode.ReadOnly() {
		opts = append(opts, llms.WithTools(toolkit.ReadOnlyTools()...))
	} else {
		opts = append(opts, llms.WithToolkits(toolkit))
	}
	if mainCfg.ReasoningEffort != "" {
		opts = append(opts, llms.WithReasoningEffort(mainCfg.ReasoningEffort))
	}

	if agentCtx.OnProgress != nil {
		opts = append(opts, llms.WithStreamingFunc(c.handleStreamingEvent))
	}

	mainModel := models.Classes[ModelMain]
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
	models   Models
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

	content := c.agentCtx.PullRequest
	var repoInstructions string
	var recentCommits []string
	if rc := c.agentCtx.RepoContext; rc != nil {
		repoInstructions = rc.Instructions
		recentCommits = rc.RecentCommits
	}

	// The summary uses the accumulated message history plus a trailing user
	// turn that requests the commit/PR summary. We do not feed the session's
	// tool list here — a one-shot structured-output call has no need for
	// tools, and the API requires the transcript to end with a user message.
	system := summarySystemPrompt(content, repoInstructions)
	messages := append(c.sess.Messages(),
		llms.NewMessage(llms.RoleUser, llms.NewTextPart(summaryPrompt(content, recentCommits))))

	fallback := func() *agent.Result {
		return &agent.Result{
			Title:       "Apply agent changes",
			Description: "Automated changes by kvarn agent.",
			Cost:        finalCost(),
		}
	}

	summary, raw, err := c.requestSummary(ctx, system, messages)
	if err != nil {
		slog.Warn("failed to generate summary, using default", "error", err)
		return fallback(), nil
	}

	// A title that does not name the change, a title over budget, or a missing
	// required section cannot be fixed on the host without inventing text, so
	// the model gets one more chance with the problems named. One retry, not a
	// loop: the second answer is either better or the configuration is asking
	// for something the run cannot supply, and both the title and the
	// placeholder below degrade honestly.
	if problems := summaryProblems(*summary, content); len(problems) > 0 {
		slog.Info("summary did not meet the configured requirements; asking again", "problems", problems)
		retryMessages := append(messages,
			llms.NewMessage(llms.RoleAssistant, llms.NewTextPart(raw)),
			llms.NewMessage(llms.RoleUser, llms.NewTextPart(retryPrompt(problems))))
		retried, _, retryErr := c.requestSummary(ctx, system, retryMessages)
		if retryErr != nil {
			slog.Warn("failed to regenerate summary; using the first one", "error", retryErr)
		} else {
			if remaining := summaryProblems(*retried, content); len(remaining) > 0 {
				slog.Warn("summary still does not meet the configured requirements", "problems", remaining)
			}
			summary = retried
		}
	}

	// A title that survives the retry as filler is worse than no title at all:
	// it goes on the pull request and the commit, where it stays. The generic
	// fallback at least says what the run was. The body is kept either way —
	// its problems, if any, are already rendered as such.
	title := strings.TrimSpace(summary.Title)
	if title == "" || stubTitle(title) {
		slog.Warn("summary title is still a placeholder; using the default title", "title", title)
		title = fallback().Title
	}

	return &agent.Result{
		Title:       title,
		Description: renderBody(summary.Description, content.BodySections, summary.Sections),
		Cost:        finalCost(),
	}, nil
}

// requestSummary makes one structured-output call and returns the parsed
// summary alongside the raw JSON, which a retry feeds back as the assistant
// turn it is correcting.
//
// The call runs at the main loop's reasoning effort. Constrained generation
// leaves no room to think inside the answer — every token emitted is already
// part of the JSON — so reasoning beforehand is the only chance the model gets
// to work out what the change was before it has to write it down.
func (c *codingConversation) requestSummary(
	ctx context.Context, system string, messages []*llms.Message,
) (*AgentSummary, string, error) {
	opts := []llms.GenerateOption{
		llms.WithSystemPrompt(system),
		llms.WithMessages(messages...),
		llms.WithResponseSchema[AgentSummary](),
		llms.WithMaxOutputTokens(summaryMaxOutputTokens),
	}
	if c.mainCfg.ReasoningEffort != "" {
		opts = append(opts, llms.WithReasoningEffort(c.mainCfg.ReasoningEffort))
	}

	mainModel := c.models.Classes[ModelMain]
	result, err := mainModel.GenerateContent(ctx, opts...)
	if c.agentCtx.Cost != nil {
		c.agentCtx.Cost.CheckBudget()
	}
	if err != nil {
		return nil, "", err
	}
	structured, ok := result.(llms.StructuredResult[AgentSummary])
	if !ok {
		return nil, "", fmt.Errorf("summary call returned %T, want a structured result", result)
	}
	return &structured.Data, structured.Raw, nil
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
