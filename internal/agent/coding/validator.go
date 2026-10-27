package coding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// ValidatorAgentName is the agent that checks prompt validation steps. It is
// configured in agents.toml like a sub-agent, under [agents.validator], but the
// coding agent cannot spawn it: a check that the agent under check could call
// would not be independent of it.
const ValidatorAgentName = "validator"

// validatorClass is the class the validator runs on unless configuration
// moves it. A verdict decides whether the run goes round again, so a wrong one
// costs a whole agent pass, and a check runs once per attempt rather than once
// per step.
const validatorClass = ClassReasoning

// verdictMaxOutputTokens bounds the closing verdict call. The findings are a
// short list, but the call reasons before it answers and that comes from the
// same allowance.
const verdictMaxOutputTokens = 8192

// cleanupTimeout bounds undoing what a check left behind. It runs on a
// context of its own, because a check that ran out of time must still not
// leave its scratch files in the delivered change.
const cleanupTimeout = 2 * time.Minute

// PromptVerdict is the structured output of the verdict call.
//
// Findings come before Passed for the same reason the summary's title comes
// last: constrained generation fills properties in schema order and cannot go
// back, so the model states its evidence before it commits to a verdict.
//
// Descriptions must not contain a comma; see AgentSummary.
type PromptVerdict struct {
	Findings []string `json:"findings" jsonschema:"description=Each problem found against the criterion as one self-contained item: what is wrong and where (file and line or the command that showed it) and what was observed. Empty when nothing is wrong."`
	Passed   bool     `json:"passed"   jsonschema:"description=True when the change meets the criterion. False only for a problem listed in findings."`
}

// validatorState is what a conversation remembers between checks: the findings
// each step gave last time, so the next check can confirm they were dealt
// with before it looks for anything new.
type validatorState struct {
	mu       sync.Mutex
	previous map[string][]string
}

func (s *validatorState) get(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.previous[key]
}

func (s *validatorState) set(key string, findings []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.previous == nil {
		s.previous = make(map[string][]string)
	}
	s.previous[key] = findings
}

// ValidatePrompt checks the workspace against one prompt step in an agent of
// its own. The agent gets the task and the criterion, not the conversation
// that made the change, and it gets read-only tools plus a shell. Whatever the
// shell writes is put back when the check ends, so the check can try the
// change out without becoming part of it.
//
// A check that cannot be cleaned up returns an error, whatever its verdict
// was, because what it wrote would otherwise be delivered. So does a check
// whose model stays unavailable. A check that runs past the step's timeout
// returns an error that wraps sandbox.ErrPromptTimeout.
func (c *codingConversation) ValidatePrompt(ctx context.Context, check agent.PromptCheck) (result sandbox.PromptVerdict, err error) {
	model, ok := c.models.Agents[ValidatorAgentName]
	if !ok {
		return sandbox.PromptVerdict{}, errors.New("the validator agent has no resolved model")
	}
	cfg := c.models.AgentConfigs[ValidatorAgentName]
	if c.agentCtx.Cost != nil {
		ctx = llms.WithMetrics(ctx, c.agentCtx.Cost.Recorder())
	}
	stepCtx := ctx
	if check.Step.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(check.Step.Timeout))
		defer cancel()
	}
	// timedOut turns an error caused by the step's own timeout into one that
	// fails the step, and leaves every other error alone.
	timedOut := func(err error) error {
		if ctx.Err() != nil && stepCtx.Err() == nil {
			return fmt.Errorf("%w (%s)", sandbox.ErrPromptTimeout, time.Duration(check.Step.Timeout))
		}
		return err
	}

	runner := c.agentCtx.Runner
	workDir := c.agentCtx.WorkingDir

	// Without a record of the workspace and of the processes running in the
	// guest there is no way to undo what the check does, so no check runs: a
	// verdict is not worth a workspace nobody can vouch for.
	tree, err := sandbox.SnapshotWorktree(ctx, runner, workDir)
	if err != nil {
		return sandbox.PromptVerdict{}, timedOut(err)
	}
	processes, err := sandbox.UserProcesses(ctx, runner)
	if err != nil {
		return sandbox.PromptVerdict{}, timedOut(err)
	}

	sessResp, err := runner.CreateSession(ctx, &v1.CreateSessionRequest{WorkingDir: workDir})
	if err != nil {
		return sandbox.PromptVerdict{}, timedOut(fmt.Errorf("create validator session: %w", err))
	}
	shellID := sessResp.SessionId
	defer func() {
		cerr := c.cleanUpAfterCheck(ctx, check.Step.Name, shellID, processes, tree)
		switch {
		case cerr == nil:
		case err == nil:
			slog.Error("could not clean up after a prompt check", "step", check.Step.Name, "error", cerr)
			err = cerr
		default:
			// The cleanup error is the one kept for errors.Is: a check that
			// timed out and then could not be cleaned up must stop the run,
			// not just fail its step.
			slog.Error("could not clean up after a prompt check", "step", check.Step.Name, "error", cerr)
			err = fmt.Errorf("%w (the check had already failed: %v)", cerr, err)
		}
	}()

	runID, err := newRunID()
	if err != nil {
		return sandbox.PromptVerdict{}, fmt.Errorf("generate run id: %w", err)
	}
	ctx = llms.WithStreamScope(ctx, llms.StreamScope{
		AgentID: ValidatorAgentName + "/" + runID,
		RunID:   runID,
	})

	var skills []repocontext.Skill
	if rc := c.agentCtx.RepoContext; rc != nil {
		skills = rc.Skills
	}
	toolkit := NewCodingToolkitWithOpts(CodingToolkitOpts{
		Runner:     runner,
		WorkingDir: workDir,
		SessionID:  shellID,
		Skills:     skills,
		RepoCtx:    c.agentCtx.RepoContext,
		Tracker:    c.agentCtx.Cost,
	})

	key := check.Step.Name + "\x00" + check.Step.Prompt
	previous := c.validator.get(key)

	maxOut := cfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = 16384
	}
	maxSteps := cfg.MaxSteps
	if maxSteps == 0 {
		maxSteps = 50
	}
	opts := []llms.GenerateOption{
		llms.WithSystemPrompt(validatorSystemPrompt(c.agentCtx.ProjectName, check.BaseCommit)),
		llms.WithMessages(llms.NewMessage(llms.RoleUser, llms.NewTextPart(
			validatorTaskPrompt(c.agentCtx.Prompt, check.Step.Name, check.Step.Prompt, previous)))),
		llms.WithTools(toolkit.ReadOnlyTools()...),
		llms.WithMaxSteps(maxSteps),
		llms.WithMaxOutputTokens(maxOut),
		llms.WithToolCallTimeout(hostToolCallTimeout),
	}
	opts = append(opts, retryOptions(cfg, c.handleRetry)...)
	if cfg.ReasoningEffort != "" {
		opts = append(opts, llms.WithReasoningEffort(cfg.ReasoningEffort))
	}
	if c.agentCtx.OnProgress != nil {
		opts = append(opts, llms.WithStreamingFunc(c.handleStreamingEvent))
	}

	sess, err := llms.NewSession(ctx, model, opts...)
	if err != nil {
		return sandbox.PromptVerdict{}, timedOut(fmt.Errorf("start validator: %w", err))
	}
	// A model that stays unavailable through every attempt llms-go makes gets
	// one more round, here. The failed turn leaves the conversation as it was,
	// so the check continues where it stopped, and the coding agent is not
	// sent round again for a problem that is not in its change.
	retried := false
	for {
		_, done, stepErr := sess.Step(ctx)
		if stepErr != nil {
			c.checkBudget()
			if llms.IsRetryable(stepErr) && !retried && ctx.Err() == nil {
				retried = true
				slog.Warn("the validator model is unavailable; trying the check once more", "step", check.Step.Name, "error", stepErr)
				continue
			}
			return sandbox.PromptVerdict{}, timedOut(fmt.Errorf("validator: %w", stepErr))
		}
		if done {
			break
		}
	}
	c.checkBudget()

	verdict, err := c.requestVerdict(ctx, model, cfg.ReasoningEffort, cfg.MaxAttempts, sess.Messages())
	if err != nil && llms.IsRetryable(err) && ctx.Err() == nil {
		slog.Warn("the validator model is unavailable; asking for the verdict once more", "step", check.Step.Name, "error", err)
		verdict, err = c.requestVerdict(ctx, model, cfg.ReasoningEffort, cfg.MaxAttempts, sess.Messages())
	}
	if err != nil {
		return sandbox.PromptVerdict{}, timedOut(err)
	}
	findings := cleanFindings(verdict.Findings)
	// Only a failed check's findings reach the coding agent, so only those
	// are given to the next check as problems the agent was asked to fix.
	if verdict.Passed {
		c.validator.set(key, nil)
	} else {
		c.validator.set(key, findings)
	}

	return sandbox.PromptVerdict{
		Passed:   verdict.Passed,
		Findings: renderFindings(verdict.Passed, findings),
	}, nil
}

// cleanUpAfterCheck undoes what a check left behind, in the order that keeps
// it undone. The check's shell goes first, and with it its process group.
// Then every process that started during the check and left that group. Only
// then is the workspace put back, so nothing the check started is still
// writing to it.
//
// Every failure is an error. A process that could not be stopped can still
// write to the workspace after it was put back.
func (c *codingConversation) cleanUpAfterCheck(ctx context.Context, step, shellID string, keep []string, tree string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	runner := c.agentCtx.Runner

	// A shell that does not close is not an error of its own: its processes
	// started during the check, so the next call stops them.
	_, _ = runner.CloseSession(ctx, &v1.CloseSessionRequest{SessionId: shellID})

	killed, err := sandbox.KillProcessesExcept(ctx, runner, keep)
	if err != nil {
		return err
	}
	if killed > 0 {
		slog.Info("stopped processes a prompt check left running", "step", step, "count", killed)
	}

	restored, err := sandbox.RestoreWorktree(ctx, runner, c.agentCtx.WorkingDir, tree)
	if err != nil {
		return err
	}
	if restored {
		slog.Info("put the workspace back after a prompt check changed it", "step", step)
	}
	return nil
}

// requestVerdict asks for the verdict as structured output, after the agent
// has finished looking. It is a separate call because a verdict has to be
// read by code: free text that says the change "looks fine" is not one.
func (c *codingConversation) requestVerdict(
	ctx context.Context, model llms.Model, effort llms.Effort, maxAttempts int, history []*llms.Message,
) (*PromptVerdict, error) {
	messages := append(history, llms.NewMessage(llms.RoleUser, llms.NewTextPart(verdictPrompt)))
	opts := []llms.GenerateOption{
		llms.WithSystemPrompt("You give the verdict of a validation check from the investigation in this conversation. Report only what the investigation showed."),
		llms.WithMessages(messages...),
		llms.WithResponseSchema[PromptVerdict](),
		llms.WithMaxOutputTokens(verdictMaxOutputTokens),
	}
	if maxAttempts > 0 {
		opts = append(opts, llms.WithMaxRetries(maxAttempts-1))
	}
	if effort != "" {
		opts = append(opts, llms.WithReasoningEffort(effort))
	}
	result, err := model.GenerateContent(ctx, opts...)
	c.checkBudget()
	if err != nil {
		return nil, fmt.Errorf("validator verdict: %w", err)
	}
	structured, ok := result.(llms.StructuredResult[PromptVerdict])
	if !ok {
		return nil, fmt.Errorf("validator verdict returned %T, want a structured result", result)
	}
	return &structured.Data, nil
}

func (c *codingConversation) checkBudget() {
	if c.agentCtx.Cost != nil {
		c.agentCtx.Cost.CheckBudget()
	}
}

// cleanFindings drops empty items, so an empty string in the model's list
// does not read as a finding.
func cleanFindings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// renderFindings writes the findings as the step's output. A failed check
// with no findings says so, because an agent told to fix something it is not
// told about can only guess.
func renderFindings(passed bool, findings []string) string {
	if len(findings) == 0 {
		if passed {
			return ""
		}
		return "The validator failed the check without naming a problem."
	}
	var sb strings.Builder
	for i, f := range findings {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("- ")
		sb.WriteString(strings.ReplaceAll(f, "\n", "\n  "))
	}
	return sb.String()
}

// verdictPrompt closes the investigation and asks for the verdict.
const verdictPrompt = "Give your verdict on the criterion now, from what you found. List each problem as a finding. Pass the change if it meets the criterion."

// validatorSystemPrompt sets out how the validator works. baseCommit is where
// the change starts; empty means the workspace has no recorded start, and the
// change is then measured against HEAD.
func validatorSystemPrompt(projectName, baseCommit string) string {
	base := baseCommit
	if base == "" {
		base = "HEAD"
	}
	var sb strings.Builder
	sb.WriteString("You are a validation agent for the project ")
	sb.WriteString(projectName)
	sb.WriteString(`. A coding agent changed this workspace to do a task. You check the change against one criterion and give a verdict. You do not fix anything.

## How to work

- See the change: ` + "`git diff " + base + "`" + ` shows the changes to tracked files, and ` + "`git status --short`" + ` also lists new files that the diff does not show.
- Check the criterion with evidence: read the code, and run builds, tests or scripts when they can show whether the criterion is met. Do not trust what the change says about itself.
- You can write scratch files and scripts to try the change out. Every change you make to the workspace is removed when you finish, so you cannot fix the change and you cannot break it. Processes you start are stopped when you finish.
- Leave everything outside the workspace as you found it. Databases, running services and files elsewhere on this machine are not reset after you, and the coding agent and the next checks use them. Read data rather than change it where you can. If you must change data to check the criterion, remove it again before you finish.
- Check only the criterion. Do not fail the change for a problem the criterion does not ask about.
- Stop when you have enough evidence for a verdict. Then write a short summary of what you checked and what you found.

## Findings

Each finding must be something the coding agent can act on without asking: what is wrong, where (a file and line, or the command that showed it), and what you observed. Do not list style preferences or ideas for improvement as findings.`)
	return sb.String()
}

// validatorTaskPrompt is the validator's first message: the task the change
// was made for, the criterion, and — on a later attempt — the findings the
// last check gave. The earlier findings come first because the coding agent
// has had one pass to deal with them, and a check that finds new problems
// each time it runs never lets the run finish.
func validatorTaskPrompt(task, name, criterion string, previous []string) string {
	var sb strings.Builder
	sb.WriteString("## The task the coding agent was given\n\n")
	sb.WriteString(strings.TrimSpace(task))
	sb.WriteString("\n\n## Criterion: ")
	sb.WriteString(name)
	sb.WriteString("\n\n")
	sb.WriteString(strings.TrimSpace(criterion))
	sb.WriteString("\n")
	if len(previous) > 0 {
		sb.WriteString("\n## Findings from your last check\n\n")
		sb.WriteString("The coding agent has changed the workspace since then to deal with these. Check each one first. Add a new finding only for a severe problem that the criterion asks about.\n\n")
		sb.WriteString(renderFindings(false, previous))
		sb.WriteString("\n")
	}
	return sb.String()
}
