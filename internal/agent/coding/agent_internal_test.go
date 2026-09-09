package coding

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

var _ = Describe("conversation progress", func() {
	var (
		conv   *codingConversation
		events []agent.ProgressEvent
	)

	BeforeEach(func() {
		events = nil
		conv = &codingConversation{
			agentCtx: &agent.Context{
				OnProgress: func(e agent.ProgressEvent) { events = append(events, e) },
			},
			mainCfg:  modelcfg.Entry{ModelID: "test/medium", MaxAttempts: 4},
			textBufs: make(map[string]*strings.Builder),
			turns:    make(map[string]*turnState),
		}
	})

	feed := func(ctx context.Context, evs ...llms.StreamingEvent) {
		for _, e := range evs {
			Expect(conv.handleStreamingEvent(ctx, e)).To(Succeed())
		}
	}

	It("brackets a turn that only calls tools", func() {
		// The model produced no text, so nothing but the turn events says the
		// step happened at all.
		feed(context.Background(),
			llms.StreamingEventMessageStart{},
			llms.StreamingEventMessageEnd{},
		)

		Expect(events).To(Equal([]agent.ProgressEvent{
			agent.ProgressTurn{Phase: agent.TurnStarted, Step: 1, Model: "test/medium"},
			agent.ProgressTurn{Phase: agent.TurnEnded, Step: 1, Model: "test/medium"},
		}))
	})

	It("reports the first token once, whether it is thinking or text", func() {
		feed(context.Background(),
			llms.StreamingEventMessageStart{},
			llms.StreamingEventThinking{Text: "hmm"},
			llms.StreamingEventTextChunk{Text: "he"},
			llms.StreamingEventTextChunk{Text: "llo"},
			llms.StreamingEventMessageEnd{Final: true},
		)

		Expect(events).To(Equal([]agent.ProgressEvent{
			agent.ProgressTurn{Phase: agent.TurnStarted, Step: 1, Model: "test/medium"},
			agent.ProgressTurn{Phase: agent.TurnResponding, Step: 1, Model: "test/medium"},
			agent.ProgressTextMessage{Text: "hello", Final: true},
			agent.ProgressTurn{Phase: agent.TurnEnded, Step: 1, Final: true, Model: "test/medium"},
		}))
	})

	It("counts steps per agent, and reports no model for a sub-agent", func() {
		sub := llms.WithStreamScope(context.Background(), llms.StreamScope{AgentID: "plan/ab12"})

		feed(context.Background(), llms.StreamingEventMessageStart{}, llms.StreamingEventMessageEnd{})
		feed(sub, llms.StreamingEventMessageStart{}, llms.StreamingEventMessageEnd{})
		feed(context.Background(), llms.StreamingEventMessageStart{})

		Expect(events).To(ContainElement(agent.ProgressTurn{
			AgentID: "plan/ab12", Phase: agent.TurnStarted, Step: 1,
		}))
		Expect(events[len(events)-1]).To(Equal(agent.ProgressTurn{
			Phase: agent.TurnStarted, Step: 2, Model: "test/medium",
		}))
	})

	It("attaches a retry to the model call it interrupted", func() {
		feed(context.Background(), llms.StreamingEventMessageStart{}, llms.StreamingEventMessageEnd{})
		feed(context.Background(), llms.StreamingEventMessageStart{})
		events = nil

		conv.handleRetry(context.Background(), llms.RetryNotice{
			Provider:    "anthropic",
			Model:       "claude-sonnet-4-6",
			Attempt:     1,
			MaxAttempts: 4,
			Delay:       500 * time.Millisecond,
			StatusCode:  529,
			Err:         errors.New("overloaded"),
		})

		Expect(events).To(Equal([]agent.ProgressEvent{
			agent.ProgressRetry{
				Step:        2,
				Attempt:     1,
				MaxAttempts: 4,
				Delay:       500 * time.Millisecond,
				StatusCode:  529,
				Provider:    "anthropic",
				Model:       "claude-sonnet-4-6",
				Error:       "overloaded",
			},
		}))
	})

	It("reports a summary retry without a step, since it is outside the loop", func() {
		feed(context.Background(), llms.StreamingEventMessageStart{}, llms.StreamingEventMessageEnd{})
		events = nil

		conv.handleSummaryRetry(context.Background(), llms.RetryNotice{
			Provider: "anthropic", Attempt: 1, MaxAttempts: 4,
		})

		Expect(events).To(HaveLen(1))
		Expect(events[0].(agent.ProgressRetry).Step).To(BeZero())
	})
})

var _ = Describe("retryOptions", func() {
	It("turns an attempt budget into the retries that follow the first try", func() {
		Expect(retryOptions(modelcfg.Entry{MaxAttempts: 4}, nil)).To(HaveLen(1))
	})

	It("leaves the library default alone when no budget is configured", func() {
		Expect(retryOptions(modelcfg.Entry{}, nil)).To(BeEmpty())
	})

	It("registers the notify callback when one is given", func() {
		notify := func(context.Context, llms.RetryNotice) {}
		Expect(retryOptions(modelcfg.Entry{MaxAttempts: 4}, notify)).To(HaveLen(2))
	})
})
