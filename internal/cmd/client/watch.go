package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
)

// WatchSession streams a session's events to stdout/stderr until it reaches a
// terminal state. Shared by the commands that start work and then follow it.
func WatchSession(ctx context.Context, oc kvarnv1connect.OrchestratorServiceClient, sessionID string) error {
	return WatchSessionFrom(ctx, oc, sessionID, 0)
}

// WatchSessionFrom is WatchSession resumed from a cursor: only events after
// fromSeq are delivered, so a client that has already seen part of the history
// does not print it twice. Zero replays everything the store still holds.
func WatchSessionFrom(ctx context.Context, oc kvarnv1connect.OrchestratorServiceClient, sessionID string, fromSeq int64) error {
	stream, err := oc.WatchSession(ctx, &v1.WatchSessionRequest{
		SessionId:    sessionID,
		FromSequence: fromSeq,
	})
	if err != nil {
		return fmt.Errorf("watch session: %w", err)
	}
	defer stream.Close()

	printer := NewPrinter()
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("watch stream: %w", err)
		}
		printer.Print(msg)
	}

	return nil
}

// Printer renders a session's events for a human, on stdout unless the event
// reports a failure. Replaying history and following a live stream print the
// same way because they are the same events, one from the store and one from
// the hub.
//
// Every line starts with the time the event happened, in local time, so a
// person can see how long a quiet stretch has lasted. Lines that belong to a
// sub-agent run are indented under the parent and carry the run's identifier,
// because several runs can be active at once and their lines interleave.
//
// It is stateful for one reason: how long the model spent on a turn is the
// difference between two events, and the events carry the times to subtract.
type Printer struct {
	out    io.Writer
	errOut io.Writer
	loc    *time.Location
	// turnStart is when the model call currently in flight began, per agent.
	turnStart map[string]time.Time
}

// NewPrinter returns a Printer for one stream of events. Reuse it across the
// whole stream; a fresh one for each event would forget when the turn started.
func NewPrinter() *Printer {
	return newPrinter(os.Stdout, os.Stderr, time.Local)
}

func newPrinter(out, errOut io.Writer, loc *time.Location) *Printer {
	return &Printer{
		out:       out,
		errOut:    errOut,
		loc:       loc,
		turnStart: make(map[string]time.Time),
	}
}

// Print renders one session event.
func (p *Printer) Print(update *v1.SessionUpdate) {
	at := updateTime(update)
	switch e := update.Event.(type) {
	case *v1.SessionUpdate_StateChange:
		sc := e.StateChange
		if sc.Error != "" {
			p.line(p.errOut, at, "", "[%s] %s: %s", sc.State, sc.Message, sc.Error)
		} else {
			p.line(p.out, at, "", "[%s] %s", sc.State, sc.Message)
		}
	case *v1.SessionUpdate_AgentTurn:
		p.printTurn(e.AgentTurn, at)
	case *v1.SessionUpdate_AgentRetry:
		p.printRetry(e.AgentRetry, at)
	case *v1.SessionUpdate_SubAgent:
		p.printSubAgent(e.SubAgent, at)
	case *v1.SessionUpdate_AgentMessage:
		// Only the parent's final answer is printed, and without a time, so
		// it can be copied as it is. A sub-agent's answer goes to the parent,
		// and its finished line already says that it arrived.
		if e.AgentMessage.Final && e.AgentMessage.AgentId == "" {
			fmt.Fprintln(p.out, e.AgentMessage.Text)
		}
	case *v1.SessionUpdate_AgentToolUse:
		tu := e.AgentToolUse
		if tu.AgentId == "" {
			p.line(p.out, at, "", "=> %s %s", tu.ToolId, toolArguments(tu.ToolId, tu.ArgumentsJson))
		} else {
			p.line(p.out, at, tu.AgentId, "%s => %s %s", tu.AgentId, tu.ToolId, tu.ArgumentsJson)
		}
	case *v1.SessionUpdate_AgentToolResult:
		tr := e.AgentToolResult
		if !tr.IsError {
			break
		}
		if tr.AgentId == "" {
			p.line(p.errOut, at, "", "   error: %s", tr.Result)
		} else {
			p.line(p.errOut, at, tr.AgentId, "%s error: %s", tr.AgentId, tr.Result)
		}
	case *v1.SessionUpdate_PullRequestCreated:
		pr := e.PullRequestCreated
		p.line(p.out, at, "", "[pr] %s (%s)", pr.Url, pr.Branch)
	case *v1.SessionUpdate_VmInfo:
		vi := e.VmInfo
		p.line(p.out, at, "", "[vm] %d cores, %d MB memory, %d/%d MB disk",
			vi.CpuCount, vi.MemTotalMb, vi.DiskUsedMb, vi.DiskTotalMb)
	case *v1.SessionUpdate_DependencyOutput:
		// The chunks carry their own line breaks, so they are written as
		// they come rather than through line.
		do := e.DependencyOutput
		if do.Stdout != "" {
			fmt.Fprintf(p.out, "%s[deps] %s", p.stamp(at), do.Stdout)
		}
		if do.Stderr != "" {
			fmt.Fprintf(p.errOut, "%s[deps] %s", p.stamp(at), do.Stderr)
		}
	case *v1.SessionUpdate_CacheProgress:
		cp := e.CacheProgress
		action := "saving"
		if cp.Restoring {
			action = "restoring"
		}
		p.line(p.out, at, "", "[cache] %s %s (%d/%d)", action, cp.Path, cp.Index, cp.Total)
	}
}

// line writes one rendered line: the time, the indent of the agent it belongs
// to, then the text.
func (p *Printer) line(w io.Writer, at time.Time, agentID string, format string, args ...any) {
	indent := ""
	if agentID != "" {
		indent = subAgentIndent
	}
	fmt.Fprintf(w, "%s%s%s\n", p.stamp(at), indent, fmt.Sprintf(format, args...))
}

// subAgentIndent sets a sub-agent's lines apart from the parent's.
const subAgentIndent = "  "

// stamp renders the time an event happened as the start of its line. An event
// without a time gets blanks of the same width, so the text after it still
// lines up with the rest.
func (p *Printer) stamp(at time.Time) string {
	if at.IsZero() {
		return strings.Repeat(" ", len(stampLayout)+1)
	}
	return at.In(p.loc).Format(stampLayout) + " "
}

const stampLayout = "15:04:05"

// toolArguments renders a parent tool call's arguments. A spawn_agent call is
// shown as the sub-agent and the reason for it: the prompt can run to many
// lines, and the sub-agent's own lines show what it does with it.
func toolArguments(toolID, argumentsJSON string) string {
	if toolID != "spawn_agent" {
		return argumentsJSON
	}
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil || args.Name == "" {
		return argumentsJSON
	}
	if args.Description == "" {
		return args.Name
	}
	return fmt.Sprintf("%s %q", args.Name, args.Description)
}

// printTurn renders one phase of a model call. Read together, the three lines
// say where the time went: waiting for the provider, producing output, and
// then — until the next turn opens — running tools.
func (p *Printer) printTurn(turn *v1.AgentTurn, at time.Time) {
	label := turnLabel(turn.AgentId, turn.Step)
	switch turn.Phase {
	case v1.AgentTurnPhase_AGENT_TURN_PHASE_STARTED:
		if !at.IsZero() {
			p.turnStart[turn.AgentId] = at
		}
		model := turn.Model
		if model == "" {
			model = "the model"
		}
		p.line(p.out, at, turn.AgentId, "%s calling %s", label, model)
	case v1.AgentTurnPhase_AGENT_TURN_PHASE_RESPONDING:
		p.line(p.out, at, turn.AgentId, "%s responding%s", label, p.since(turn.AgentId, at))
	case v1.AgentTurnPhase_AGENT_TURN_PHASE_ENDED:
		p.line(p.out, at, turn.AgentId, "%s done%s", label, p.since(turn.AgentId, at))
		delete(p.turnStart, turn.AgentId)
	}
}

func (p *Printer) printRetry(retry *v1.AgentRetry, at time.Time) {
	label := turnLabel(retry.AgentId, retry.Step)
	status := ""
	if retry.StatusCode != 0 {
		status = fmt.Sprintf(" %d", retry.StatusCode)
	}
	provider := retry.Provider
	if provider == "" {
		provider = "the provider"
	}
	p.line(p.errOut, at, retry.AgentId, "%s attempt %d/%d failed (%s%s), retrying in %s",
		label, retry.Attempt, retry.MaxAttempts, provider, status,
		roundDuration(time.Duration(retry.DelayMs)*time.Millisecond))
}

// printSubAgent renders the start or the end of a sub-agent run. The end line
// is the one a person waits for: it says which run the parent no longer waits
// on.
func (p *Printer) printSubAgent(sa *v1.SubAgent, at time.Time) {
	label := fmt.Sprintf("[%s]", sa.AgentId)
	switch sa.Phase {
	case v1.SubAgentPhase_SUB_AGENT_PHASE_STARTED:
		if sa.Description == "" {
			p.line(p.out, at, sa.AgentId, "%s started", label)
		} else {
			p.line(p.out, at, sa.AgentId, "%s started: %s", label, sa.Description)
		}
	case v1.SubAgentPhase_SUB_AGENT_PHASE_FINISHED:
		took := fmt.Sprintf("after %s, %s",
			roundDuration(time.Duration(sa.DurationMs)*time.Millisecond), pluralSteps(sa.Steps))
		if sa.Error != "" {
			p.line(p.errOut, at, sa.AgentId, "%s failed %s: %s", label, took, firstLine(sa.Error))
		} else {
			p.line(p.out, at, sa.AgentId, "%s finished %s", label, took)
		}
	}
}

func pluralSteps(n int32) string {
	if n == 1 {
		return "1 step"
	}
	return fmt.Sprintf("%d steps", n)
}

// firstLine returns the first non-empty line of s, trimmed. An error can carry
// a whole response body, and one line is enough to say what went wrong.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// since renders how long the current turn has run, or nothing when either end
// of the measurement is missing.
func (p *Printer) since(agentID string, at time.Time) string {
	start, ok := p.turnStart[agentID]
	if !ok || at.IsZero() {
		return ""
	}
	return " after " + roundDuration(at.Sub(start)).String()
}

// turnLabel prefixes a line with the agent and the model call it belongs to. A
// step of 0 marks a call outside the agent loop, such as the closing summary.
func turnLabel(agentID string, step int32) string {
	prefix := "[llm]"
	if agentID != "" {
		prefix = fmt.Sprintf("[llm:%s]", agentID)
	}
	if step <= 0 {
		return prefix + " summary:"
	}
	return fmt.Sprintf("%s step %d:", prefix, step)
}

// roundDuration trims a duration to something a person reads at a glance.
func roundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(100 * time.Millisecond)
}

// updateTime returns when the event happened, or the zero time when the server
// did not say.
func updateTime(update *v1.SessionUpdate) time.Time {
	if update.Timestamp == nil {
		return time.Time{}
	}
	return update.Timestamp.AsTime()
}
