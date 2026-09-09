package client

import (
	"context"
	"fmt"
	"os"
	"time"

	"connectrpc.com/connect"
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
	stream, err := oc.WatchSession(ctx, connect.NewRequest(&v1.WatchSessionRequest{
		SessionId:    sessionID,
		FromSequence: fromSeq,
	}))
	if err != nil {
		return fmt.Errorf("watch session: %w", err)
	}
	defer stream.Close()

	printer := NewPrinter()
	for stream.Receive() {
		printer.Print(stream.Msg())
	}

	if err := stream.Err(); err != nil {
		return fmt.Errorf("watch stream: %w", err)
	}
	return nil
}

// Printer renders a session's events for a human, on stdout unless the event
// reports a failure. Replaying history and following a live stream print the
// same way because they are the same events, one from the store and one from
// the hub.
//
// It is stateful for one reason: how long the model spent on a turn is the
// difference between two events, and the events carry the times to subtract.
type Printer struct {
	// turnStart is when the model call currently in flight began, per agent.
	turnStart map[string]time.Time
}

// NewPrinter returns a Printer for one stream of events. Reuse it across the
// whole stream; a fresh one for each event would forget when the turn started.
func NewPrinter() *Printer {
	return &Printer{turnStart: make(map[string]time.Time)}
}

// Print renders one session event.
func (p *Printer) Print(update *v1.SessionUpdate) {
	switch e := update.Event.(type) {
	case *v1.SessionUpdate_StateChange:
		sc := e.StateChange
		if sc.Error != "" {
			fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", sc.State, sc.Message, sc.Error)
		} else {
			fmt.Fprintf(os.Stdout, "[%s] %s\n", sc.State, sc.Message)
		}
	case *v1.SessionUpdate_AgentTurn:
		p.printTurn(e.AgentTurn, updateTime(update))
	case *v1.SessionUpdate_AgentRetry:
		p.printRetry(e.AgentRetry)
	case *v1.SessionUpdate_AgentMessage:
		if e.AgentMessage.Final {
			fmt.Fprintln(os.Stdout, e.AgentMessage.Text)
		}
	case *v1.SessionUpdate_AgentToolUse:
		fmt.Fprintf(os.Stdout, "=> %s %s\n", e.AgentToolUse.ToolId, e.AgentToolUse.ArgumentsJson)
	case *v1.SessionUpdate_AgentToolResult:
		if e.AgentToolResult.IsError {
			fmt.Fprintf(os.Stderr, "   error: %s\n", e.AgentToolResult.Result)
		}
	case *v1.SessionUpdate_PullRequestCreated:
		pr := e.PullRequestCreated
		fmt.Fprintf(os.Stdout, "[pr] %s (%s)\n", pr.Url, pr.Branch)
	case *v1.SessionUpdate_VmInfo:
		vi := e.VmInfo
		fmt.Fprintf(os.Stdout, "[vm] %d cores, %d MB memory, %d/%d MB disk\n",
			vi.CpuCount, vi.MemTotalMb, vi.DiskUsedMb, vi.DiskTotalMb)
	case *v1.SessionUpdate_DependencyOutput:
		do := e.DependencyOutput
		if do.Stdout != "" {
			fmt.Fprintf(os.Stdout, "[deps] %s", do.Stdout)
		}
		if do.Stderr != "" {
			fmt.Fprintf(os.Stderr, "[deps] %s", do.Stderr)
		}
	case *v1.SessionUpdate_CacheProgress:
		cp := e.CacheProgress
		action := "saving"
		if cp.Restoring {
			action = "restoring"
		}
		fmt.Fprintf(os.Stdout, "[cache] %s %s (%d/%d)\n", action, cp.Path, cp.Index, cp.Total)
	}
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
		fmt.Fprintf(os.Stdout, "%s calling %s\n", label, model)
	case v1.AgentTurnPhase_AGENT_TURN_PHASE_RESPONDING:
		fmt.Fprintf(os.Stdout, "%s responding%s\n", label, p.since(turn.AgentId, at))
	case v1.AgentTurnPhase_AGENT_TURN_PHASE_ENDED:
		fmt.Fprintf(os.Stdout, "%s done%s\n", label, p.since(turn.AgentId, at))
		delete(p.turnStart, turn.AgentId)
	}
}

func (p *Printer) printRetry(retry *v1.AgentRetry) {
	label := turnLabel(retry.AgentId, retry.Step)
	status := ""
	if retry.StatusCode != 0 {
		status = fmt.Sprintf(" %d", retry.StatusCode)
	}
	provider := retry.Provider
	if provider == "" {
		provider = "the provider"
	}
	fmt.Fprintf(os.Stderr, "%s attempt %d/%d failed (%s%s), retrying in %s\n",
		label, retry.Attempt, retry.MaxAttempts, provider, status,
		roundDuration(time.Duration(retry.DelayMs)*time.Millisecond))
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
