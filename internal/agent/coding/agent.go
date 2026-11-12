package coding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

var _ agent.PullRequestReviser = (*codingConversation)(nil)

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
	Description string           `json:"description" jsonschema:"description=Commit body and pull request description: why the change was made and what the code does now. Present tense. Leave out how it was tested and the process that produced it. Wrap lines at ~72 chars. No list of files. No trailing footer."`
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

// Start opens a stateful conversation so the orchestrator can drive multiple
// agent turns through it. The conversation preserves message history (tool
// calls and results) across turns, which is what makes validation-failure
// retries productive — the agent sees what it tried last time.
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
	if mode.ReadOnly() {
		subAgents = subAgents.ReadOnly()
	}

	// The merge tools are offered only when the run has both halves: a branch
	// shipped into the guest and somewhere for the resulting commit to go. A
	// read-only mode gets neither, since ReadOnlyTools never carries them.
	mergeTarget := agentCtx.MergeTarget
	checkpointer := agentCtx.Checkpointer
	if mergeTarget == nil || checkpointer == nil || mode.ReadOnly() {
		mergeTarget, checkpointer = nil, nil
	}

	mainCfg := models.ClassConfigs[ModelMain]
	c := &codingConversation{
		models:    models,
		agentCtx:  agentCtx,
		mode:      mode,
		mainCfg:   mainCfg,
		textBufs:  make(map[string]*strings.Builder),
		turns:     make(map[string]*turnState),
		subModels: make(map[string]string),
	}

	// Sub-agent runs report through the same handlers as the parent loop, so
	// their turns, tool calls and retries reach the watcher in the same shape.
	var progress *SubAgentProgress
	if agentCtx.OnProgress != nil {
		progress = &SubAgentProgress{
			Stream:    c.handleStreamingEvent,
			Retry:     c.handleRetry,
			Lifecycle: c.handleSubAgent,
		}
	}

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
		HeadBranch:   agentCtx.Branch,
		MergeTarget:  mergeTarget,
		Checkpointer: checkpointer,
		Progress:     progress,
		ViewModel:    models.Classes[ModelMain],
	})
	systemPrompt := mode.SystemPrompt(agentCtx.ProjectName, agentCtx.RepoURL, agentCtx.Branch, agentCtx.RepoContext, subAgents, agentCtx.PullRequest)
	if mergeTarget != nil {
		systemPrompt += MergeInstructions(mergeTarget.Branch)
	}

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
		llms.WithMaxSteps(maxSteps),
		llms.WithMaxOutputTokens(maxOut),
		llms.WithToolCallTimeout(hostToolCallTimeout),
	}
	opts = append(opts, retryOptions(mainCfg, c.handleRetry)...)
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
	c.start = func(ctx context.Context, msgs ...*llms.Message) (agentSession, error) {
		sessOpts := append(slices.Clone(opts), llms.WithMessages(msgs...))
		return llms.NewSession(ctx, mainModel, sessOpts...)
	}

	sessCtx := ctx
	if agentCtx.Cost != nil {
		sessCtx = llms.WithMetrics(sessCtx, agentCtx.Cost.Recorder())
	}
	sess, err := c.start(sessCtx, firstMessage(agentCtx.Prompt, agentCtx.PromptAttachments))
	if err != nil {
		return nil, err
	}
	c.sess = sess

	return c, nil
}

// agentSession is the part of llms.Session the conversation drives. Only a
// provider can build a real one, so the interface is what lets the retry
// handover be tested.
type agentSession interface {
	Step(ctx context.Context) (llms.StepInfo, bool, error)
	Result() (llms.Result, error)
	Messages() []*llms.Message
	Inject(msgs ...*llms.Message)
}

// codingConversation drives an llms session across one or more agent turns.
// The streaming-event handler reuses the same per-agent text buffers across
// calls so partial-message text never leaks between turns.
type codingConversation struct {
	models   Models
	agentCtx *agent.Context
	mode     *Mode
	mainCfg  modelcfg.Entry
	sess     agentSession
	// start opens a session seeded with msgs, carrying the same system prompt,
	// tools and model options every turn of this conversation runs on.
	start func(ctx context.Context, msgs ...*llms.Message) (agentSession, error)

	streamMu sync.Mutex
	textBufs map[string]*strings.Builder
	turns    map[string]*turnState
	// subModels maps a running sub-agent to the model it calls, so its turn
	// events can name that model and not the parent's.
	subModels map[string]string

	// validator carries what prompt checks remember between attempts.
	validator validatorState

	// blocks is the content policy recovery of the main loop. It lives as
	// long as the conversation, so the steer limit covers the whole job and
	// not each validation pass.
	blocks blockRecovery
}

// turnState is what the conversation remembers about the model call currently
// in flight for one agent scope: which call it is, and whether the first token
// has arrived. Both are needed because llms-go reports the start of a call and
// its content as separate events, and a retry notice has to name the call it
// belongs to.
type turnState struct {
	step      int
	responded bool
}

// turn returns the state for an agent scope, creating it on first use.
// Callers must hold streamMu.
func (c *codingConversation) turn(agentID string) *turnState {
	t, ok := c.turns[agentID]
	if !ok {
		t = &turnState{}
		c.turns[agentID] = t
	}
	return t
}

// modelStallTimeout is how long a model response that has started may stay
// silent before the attempt fails and is retried. Without it, a connection
// that dies mid-response blocks the job until the OS gives up on the TCP
// connection, and nothing reports the hang.
//
// The budget counts bytes from the provider, and provider keepalives are
// bytes, so a model that reasons for minutes without tokens does not reach it.
// The value is well above the keepalive interval of every supported provider.
const modelStallTimeout = 120 * time.Second

// modelResponseTimeout is how long an attempt may wait for the provider to
// start its response. The stall timeout only starts to count when the response
// has started, so without this budget a request that the provider accepts but
// never answers waits forever.
//
// HTTP/2 health pings do not find that case. The nearest HTTP/2 server answers
// the pings, and for a hosted provider that is a CDN or a load balancer, which
// can stay healthy while the model backend behind it is stuck.
//
// A streaming provider sends its response headers when the stream opens,
// normally within a few seconds, so this value only stops a request that is
// really stuck.
const modelResponseTimeout = 90 * time.Second

// streamingProviders are the providers that always stream a response. For
// these, the response timeout covers only the time to open the stream. Other
// providers do not stream when no streaming callback is set, and then send
// nothing until the answer is complete, so the same budget would limit the
// whole generation and fail a slow but healthy answer.
var streamingProviders = map[string]bool{
	string(llms.GenAISystemOpenAI):    true,
	string(llms.GenAISystemAnthropic): true,
}

// alwaysStreams reports whether the model ID names a provider in
// streamingProviders. An ID without a provider prefix (an alias) gives false,
// because the provider behind it is not known here.
func alwaysStreams(modelID string) bool {
	provider, _, ok := strings.Cut(modelID, "/")
	return ok && streamingProviders[provider]
}

// retryOptions builds the retry half of a generate call: how many attempts the
// model gets, when a silent or unanswered request counts as a failed attempt,
// and where a failed one is reported. notify may be nil for a call nobody is
// watching.
//
// Retries are configured per model entry rather than left at the llms-go
// default because a job is long-running and unattended: a provider that is
// briefly unavailable should cost the run a pause, not the whole conversation
// that has been built up to that point.
func retryOptions(cfg modelcfg.Entry, notify llms.RetryNotifyFunc) []llms.GenerateOption {
	opts := []llms.GenerateOption{llms.WithStallTimeout(modelStallTimeout)}
	if alwaysStreams(cfg.ModelID) {
		opts = append(opts, llms.WithRequestTimeout(modelResponseTimeout))
	}
	if cfg.MaxAttempts > 0 {
		opts = append(opts, llms.WithMaxRetries(cfg.MaxAttempts-1))
	}
	if notify != nil {
		opts = append(opts, llms.WithRetryNotify(notify))
	}
	return opts
}

// handleRetry reports a failed attempt as progress. llms-go calls it on the
// goroutine that made the request, just before it waits, so the watcher learns
// about the pause while it is still happening.
func (c *codingConversation) handleRetry(ctx context.Context, notice llms.RetryNotice) {
	agentID := streamAgentID(ctx)
	c.streamMu.Lock()
	step := c.turn(agentID).step
	c.streamMu.Unlock()
	c.reportRetry(agentID, step, notice)
}

// handleSummaryRetry reports a retry of the closing summary call. It carries no
// step: the summary is a single call outside the agent loop, so a step number
// taken from that loop would point at a turn this failure has nothing to do
// with.
func (c *codingConversation) handleSummaryRetry(_ context.Context, notice llms.RetryNotice) {
	c.reportRetry("", 0, notice)
}

func (c *codingConversation) reportRetry(agentID string, step int, notice llms.RetryNotice) {
	if c.agentCtx.OnProgress == nil {
		return
	}
	errText := ""
	if notice.Err != nil {
		errText = notice.Err.Error()
	}
	c.agentCtx.OnProgress(agent.ProgressRetry{
		AgentID:     agentID,
		Step:        step,
		Attempt:     notice.Attempt,
		MaxAttempts: notice.MaxAttempts,
		Delay:       notice.Delay,
		StatusCode:  notice.StatusCode,
		Provider:    notice.Provider,
		Model:       notice.Model,
		Error:       errText,
	})
}

// markResponding reports the first token of a turn, once. Callers must hold
// streamMu.
func (c *codingConversation) markResponding(agentID string) {
	t := c.turn(agentID)
	if t.responded {
		return
	}
	t.responded = true
	c.agentCtx.OnProgress(agent.ProgressTurn{
		AgentID: agentID,
		Phase:   agent.TurnResponding,
		Step:    t.step,
		Model:   c.modelID(agentID),
	})
}

// modelID names the model an agent scope runs on. A scope this conversation
// does not know, such as a validator run, names none, because naming the
// parent's model would be worse. Callers must hold streamMu.
func (c *codingConversation) modelID(agentID string) string {
	if agentID == "" {
		return c.mainCfg.ModelID
	}
	return c.subModels[agentID]
}

// handleSubAgent reports the start or the end of a sub-agent run, and keeps
// the per-run state only while the run is alive. Without the cleanup every run
// of a long job would leave its turn state and text buffer behind.
func (c *codingConversation) handleSubAgent(event agent.ProgressSubAgent) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	switch event.Phase {
	case agent.SubAgentStarted:
		c.subModels[event.AgentID] = event.Model
	case agent.SubAgentFinished:
		delete(c.subModels, event.AgentID)
		delete(c.turns, event.AgentID)
		delete(c.textBufs, event.AgentID)
	}
	c.agentCtx.OnProgress(event)
}

// streamAgentID names the agent a streaming or retry callback belongs to:
// empty for the parent loop, the sub-agent run otherwise.
func streamAgentID(ctx context.Context) string {
	if scope, ok := llms.GetStreamScope(ctx); ok {
		return scope.AgentID
	}
	return ""
}

// Run advances the session to its next stopping point: either the assistant
// finishes its turn with a final text reply, or the step budget is exhausted.
// On the first call followup must be empty (the constructor's prompt is
// already in the message history); on later calls followup becomes the next
// user message.
func (c *codingConversation) Run(ctx context.Context, followup string) (string, error) {
	if c.agentCtx.Cost != nil {
		ctx = llms.WithMetrics(ctx, c.agentCtx.Cost.Recorder())
	}

	// A session ends for good once the model finishes a turn without calling a
	// tool, and nothing reopens it: a message added to a finished session is
	// recorded in its history and never sent to the model, so the turn would
	// come back instantly with the previous answer and the followup would go
	// unread. The next turn therefore runs on a new session seeded with the
	// whole transcript plus the followup, which also gives that turn a full
	// step budget rather than what the previous one left over.
	if followup != "" {
		history := append(c.sess.Messages(),
			llms.NewMessage(llms.RoleUser, llms.NewTextPart(followup)))
		sess, err := c.start(ctx, history...)
		if err != nil {
			return "", err
		}
		c.sess = sess
	}

	var interrupts interruptRecovery
	for {
		info, done, err := c.sess.Step(ctx)
		if err != nil {
			if c.agentCtx.Cost != nil {
				c.agentCtx.Cost.CheckBudget()
			}
			// A failed step reports the steps that completed before it.
			failedStep := info.Step + 1
			if llms.IsContentPolicy(err) {
				err = c.recoverFromBlock(ctx, &c.blocks, c.sess, failedStep, err)
			} else {
				err = c.recoverFromInterrupt(ctx, &interrupts, failedStep, err)
			}
			if err != nil {
				return "", err
			}
			continue
		}
		c.blocks.stepped()
		interrupts.stepped()
		if done {
			break
		}
	}
	if c.agentCtx.Cost != nil {
		c.agentCtx.Cost.CheckBudget()
	}

	result, err := c.sess.Result()
	if err != nil {
		var refusal *llms.RefusalError
		if errors.As(err, &refusal) {
			return "", refusalError(err)
		}
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

	summary, err := c.writeSummary(ctx, system, messages, content)
	if err != nil {
		slog.Warn("failed to generate summary, using default", "error", err)
		return fallback(), nil
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

// RevisePullRequest asks the model to revise the title and description of the
// pull request the run committed onto, so they describe the pull request as a
// whole after the run's commit.
//
// Unlike Summarize it has no generic fallback. A revision replaces text that a
// person may already have read, and a placeholder is worse than the text it
// would replace, so a failed call returns its error and the caller keeps the
// current text.
func (c *codingConversation) RevisePullRequest(ctx context.Context, in agent.PullRequestRevision) (*agent.Result, error) {
	if c.agentCtx.Cost != nil {
		ctx = llms.WithMetrics(ctx, c.agentCtx.Cost.Recorder())
	}

	content := c.agentCtx.PullRequest
	var repoInstructions string
	if rc := c.agentCtx.RepoContext; rc != nil {
		repoInstructions = rc.Instructions
	}

	system := summarySystemPrompt(content, repoInstructions)
	messages := append(c.sess.Messages(),
		llms.NewMessage(llms.RoleUser, llms.NewTextPart(revisionPrompt(content, in))))

	summary, err := c.writeSummary(ctx, system, messages, content)
	if err != nil {
		return nil, fmt.Errorf("revise pull request: %w", err)
	}

	// A title that is still filler after the retry would replace one somebody
	// chose. The current title is at worst out of date, so it stays.
	title := strings.TrimSpace(summary.Title)
	if title == "" || stubTitle(title) {
		slog.Warn("revised title is a placeholder; keeping the current title", "title", title)
		title = strings.TrimSpace(in.Title)
	}

	var report cost.Report
	if c.agentCtx.Cost != nil {
		report = c.agentCtx.Cost.Snapshot()
	}
	return &agent.Result{
		Title:       title,
		Description: renderBody(summary.Description, content.BodySections, summary.Sections),
		Cost:        report,
	}, nil
}

// writeSummary requests a structured summary and asks once more when the
// answer does not meet the configured requirements.
//
// A title that does not name the change, a title over budget, or a missing
// required section cannot be fixed on the host without inventing text, so the
// model gets one more chance with the problems named. One retry, not a loop:
// the second answer is either better or the configuration is asking for
// something the run cannot supply, and the caller's title check and the
// not-provided placeholder degrade honestly.
func (c *codingConversation) writeSummary(
	ctx context.Context, system string, messages []*llms.Message, content forgeconfig.Content,
) (*AgentSummary, error) {
	summary, raw, err := c.requestSummary(ctx, system, messages)
	if err != nil {
		return nil, err
	}
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
	return summary, nil
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
	opts = append(opts, retryOptions(c.mainCfg, c.handleSummaryRetry)...)
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
	agentID := streamAgentID(ctx)
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	switch e := event.(type) {
	case llms.StreamingEventMessageStart:
		t := c.turn(agentID)
		t.step++
		t.responded = false
		c.agentCtx.OnProgress(agent.ProgressTurn{
			AgentID: agentID,
			Phase:   agent.TurnStarted,
			Step:    t.step,
			Model:   c.modelID(agentID),
		})

	case llms.StreamingEventThinking:
		c.markResponding(agentID)

	case llms.StreamingEventTextChunk:
		c.markResponding(agentID)
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
		// Emitted whether or not the model produced text, because it is what
		// closes the turn: a step that only calls tools would otherwise leave
		// the watcher believing the model is still working.
		t := c.turn(agentID)
		c.agentCtx.OnProgress(agent.ProgressTurn{
			AgentID: agentID,
			Phase:   agent.TurnEnded,
			Step:    t.step,
			Model:   c.modelID(agentID),
			Final:   e.Final,
		})
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
