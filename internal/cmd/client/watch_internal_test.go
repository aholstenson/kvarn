package client

import (
	"bytes"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

var _ = Describe("Printer", func() {
	var (
		out, errOut bytes.Buffer
		printer     *Printer
		base        time.Time
	)

	BeforeEach(func() {
		out.Reset()
		errOut.Reset()
		printer = newPrinter(&out, &errOut, time.UTC)
		base = time.Date(2026, 11, 5, 14, 2, 3, 0, time.UTC)
	})

	at := func(offset time.Duration) *timestamppb.Timestamp {
		return timestamppb.New(base.Add(offset))
	}

	It("starts every line with the time the event happened", func() {
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(0),
			Event: &v1.SessionUpdate_AgentTurn{AgentTurn: &v1.AgentTurn{
				Phase: v1.AgentTurnPhase_AGENT_TURN_PHASE_STARTED, Step: 7, Model: "anthropic/claude-sonnet-4-6",
			}},
		})
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(8500 * time.Millisecond),
			Event: &v1.SessionUpdate_AgentTurn{AgentTurn: &v1.AgentTurn{
				Phase: v1.AgentTurnPhase_AGENT_TURN_PHASE_ENDED, Step: 7,
			}},
		})

		Expect(out.String()).To(Equal(
			"14:02:03 [llm] step 7: calling anthropic/claude-sonnet-4-6\n" +
				"14:02:11 [llm] step 7: done after 8.5s\n"))
	})

	It("keeps the columns aligned when an event has no time", func() {
		printer.Print(&v1.SessionUpdate{
			Event: &v1.SessionUpdate_StateChange{StateChange: &v1.StateChange{State: "running", Message: "thinking"}},
		})

		Expect(out.String()).To(Equal("         [running] thinking\n"))
	})

	It("shows a spawn_agent call as the sub-agent and the reason for it", func() {
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(0),
			Event: &v1.SessionUpdate_AgentToolUse{AgentToolUse: &v1.AgentToolUse{
				ToolId:        "spawn_agent",
				ArgumentsJson: `{"name":"explore","description":"Audit commands","prompt":"a very long prompt"}`,
			}},
		})

		Expect(out.String()).To(Equal("14:02:03 => spawn_agent explore \"Audit commands\"\n"))
	})

	It("indents a sub-agent's lines and labels them with its run", func() {
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(0),
			Event: &v1.SessionUpdate_SubAgent{SubAgent: &v1.SubAgent{
				AgentId: "explore#1", Name: "explore", Description: "Audit commands",
				Phase: v1.SubAgentPhase_SUB_AGENT_PHASE_STARTED,
			}},
		})
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(time.Second),
			Event: &v1.SessionUpdate_AgentTurn{AgentTurn: &v1.AgentTurn{
				AgentId: "explore#1", Phase: v1.AgentTurnPhase_AGENT_TURN_PHASE_STARTED, Step: 1,
				Model: "anthropic/claude-haiku-4-5",
			}},
		})
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(4 * time.Second),
			Event: &v1.SessionUpdate_AgentToolUse{AgentToolUse: &v1.AgentToolUse{
				AgentId: "explore#1", ToolId: "read_file", ArgumentsJson: `{"path":"main.go"}`,
			}},
		})
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(252 * time.Second),
			Event: &v1.SessionUpdate_SubAgent{SubAgent: &v1.SubAgent{
				AgentId: "explore#1", Name: "explore",
				Phase: v1.SubAgentPhase_SUB_AGENT_PHASE_FINISHED, Steps: 23, DurationMs: 252000,
			}},
		})

		Expect(out.String()).To(Equal(
			"14:02:03   [explore#1] started: Audit commands\n" +
				"14:02:04   [llm:explore#1] step 1: calling anthropic/claude-haiku-4-5\n" +
				"14:02:07   explore#1 => read_file {\"path\":\"main.go\"}\n" +
				"14:06:15   [explore#1] finished after 4m12s, 23 steps\n"))
	})

	It("reports a failed sub-agent run on stderr with the first line of its error", func() {
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(0),
			Event: &v1.SessionUpdate_SubAgent{SubAgent: &v1.SubAgent{
				AgentId: "plan#2", Name: "plan",
				Phase: v1.SubAgentPhase_SUB_AGENT_PHASE_FINISHED, Steps: 1, DurationMs: 900,
				Error: "sub-agent \"plan\": max steps reached\nmore detail",
			}},
		})

		Expect(out.String()).To(BeEmpty())
		Expect(errOut.String()).To(Equal(
			"14:02:03   [plan#2] failed after 900ms, 1 step: sub-agent \"plan\": max steps reached\n"))
	})

	It("prints the parent's final answer without a time and skips a sub-agent's", func() {
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(0),
			Event:     &v1.SessionUpdate_AgentMessage{AgentMessage: &v1.AgentMessage{AgentId: "explore#1", Text: "findings", Final: true}},
		})
		printer.Print(&v1.SessionUpdate{
			Timestamp: at(0),
			Event:     &v1.SessionUpdate_AgentMessage{AgentMessage: &v1.AgentMessage{Text: "All done.", Final: true}},
		})

		Expect(out.String()).To(Equal("All done.\n"))
	})
})
