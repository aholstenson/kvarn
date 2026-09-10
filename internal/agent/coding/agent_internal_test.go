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

// fakeSession is a session that ends its turn immediately, the way a real one
// does once the model answers without calling a tool.
type fakeSession struct {
	seed  []*llms.Message
	reply string
	steps int
}

func (f *fakeSession) Step(context.Context) (llms.StepInfo, bool, error) {
	f.steps++
	return llms.StepInfo{}, true, nil
}

func (f *fakeSession) Result() (llms.Result, error) {
	return llms.TextResult{Text: f.reply}, nil
}

func (f *fakeSession) Messages() []*llms.Message {
	msgs := append([]*llms.Message{}, f.seed...)
	return append(msgs, llms.NewMessage(llms.RoleAssistant, llms.NewTextPart(f.reply)))
}

var _ = Describe("conversation followup", func() {
	var (
		conv     *codingConversation
		sessions []*fakeSession
	)

	BeforeEach(func() {
		sessions = nil
		conv = &codingConversation{
			agentCtx: &agent.Context{},
			start: func(_ context.Context, msgs ...*llms.Message) (agentSession, error) {
				s := &fakeSession{seed: msgs, reply: "done"}
				sessions = append(sessions, s)
				return s, nil
			},
		}
		first, err := conv.start(context.Background(),
			llms.NewMessage(llms.RoleUser, llms.NewTextPart("do the work")))
		Expect(err).NotTo(HaveOccurred())
		conv.sess = first
	})

	text := func(m *llms.Message) string {
		return m.Parts[0].(*llms.TextPart).Text
	}

	It("carries the followup into a turn the model actually sees", func() {
		_, err := conv.Run(context.Background(), "")
		Expect(err).NotTo(HaveOccurred())

		_, err = conv.Run(context.Background(), "validation failed: build broke")
		Expect(err).NotTo(HaveOccurred())

		// A finished session never makes another model call, so the followup
		// only reaches the model as the seed of the session that replaced it.
		Expect(sessions).To(HaveLen(2))
		seed := sessions[1].seed
		Expect(sessions[1].steps).To(Equal(1))
		Expect(text(seed[len(seed)-1])).To(Equal("validation failed: build broke"))
		Expect(seed[len(seed)-1].Role).To(Equal(llms.RoleUser))
	})

	It("keeps the earlier turns so the agent iterates instead of starting over", func() {
		_, err := conv.Run(context.Background(), "")
		Expect(err).NotTo(HaveOccurred())
		_, err = conv.Run(context.Background(), "try again")
		Expect(err).NotTo(HaveOccurred())

		seed := sessions[1].seed
		Expect(seed).To(HaveLen(3))
		Expect(text(seed[0])).To(Equal("do the work"))
		Expect(text(seed[1])).To(Equal("done"))
	})

	It("stays on the same session while there is no followup", func() {
		_, err := conv.Run(context.Background(), "")
		Expect(err).NotTo(HaveOccurred())

		Expect(sessions).To(HaveLen(1))
		Expect(sessions[0].steps).To(Equal(1))
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
