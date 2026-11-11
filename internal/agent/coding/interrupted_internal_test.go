package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

var _ = Describe("model calls interrupted after output", func() {
	var (
		conv   *codingConversation
		sess   *scriptedSession
		events []agent.ProgressEvent
	)

	// interrupted is the error llms-go returns when a stream is cut after some
	// of its output reached the streaming callback.
	interrupted := func() error {
		ue := &llms.UnavailableError{
			Provider:      "openai",
			Model:         "gpt-test",
			PartialOutput: true,
			Cause:         fmt.Errorf("stream ended after 12 events: %w", llms.ErrStreamEnded),
		}
		return errors.Join(ue, llms.ErrStreamingPartialOutput)
	}
	cut := scriptedStep{err: interrupted()}
	ok := scriptedStep{}
	finish := scriptedStep{done: true}

	BeforeEach(func() {
		saved := interruptRetryBackoff
		interruptRetryBackoff = []time.Duration{0, 0}
		DeferCleanup(func() { interruptRetryBackoff = saved })

		events = nil
		conv = &codingConversation{
			agentCtx: &agent.Context{
				OnProgress: func(e agent.ProgressEvent) { events = append(events, e) },
			},
			mainCfg:   modelcfg.Entry{ModelID: "test/medium"},
			textBufs:  make(map[string]*strings.Builder),
			turns:     make(map[string]*turnState),
			subModels: make(map[string]string),
		}
		sess = &scriptedSession{conv: conv}
		conv.sess = sess
	})

	retries := func() []agent.ProgressRetry {
		var out []agent.ProgressRetry
		for _, e := range events {
			if r, ok := e.(agent.ProgressRetry); ok {
				out = append(out, r)
			}
		}
		return out
	}

	It("tries the step again", func() {
		sess.steps = []scriptedStep{cut, finish}

		text, err := conv.Run(context.Background(), "")

		Expect(err).NotTo(HaveOccurred())
		Expect(text).To(Equal("done"))
		Expect(sess.injected).To(BeEmpty())
		Expect(retries()).To(HaveLen(1))
		Expect(retries()[0].Attempt).To(Equal(1))
		Expect(retries()[0].MaxAttempts).To(Equal(3))
		Expect(retries()[0].Provider).To(Equal("openai"))
		Expect(retries()[0].Model).To(Equal("gpt-test"))
		Expect(retries()[0].Error).To(ContainSubstring("stream ended"))
	})

	It("fails with the cause when every try of a step is interrupted", func() {
		sess.steps = []scriptedStep{ok, cut, cut, cut}

		_, err := conv.Run(context.Background(), "")

		Expect(err).To(MatchError(llms.ErrStreamEnded))
		Expect(llms.IsUnavailable(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("step 2 failed 3 times"))
		Expect(retries()).To(HaveLen(2))
	})

	It("gives each step all its retries again once a step gets through", func() {
		sess.steps = []scriptedStep{cut, cut, ok, cut, cut, finish}

		_, err := conv.Run(context.Background(), "")

		Expect(err).NotTo(HaveOccurred())
		Expect(retries()).To(HaveLen(4))
	})

	It("leaves a failure without output to the retries of llms-go", func() {
		exhausted := &llms.UnavailableError{Provider: "openai", Model: "gpt-test", Attempts: 4}
		sess.steps = []scriptedStep{{err: exhausted}}

		_, err := conv.Run(context.Background(), "")

		Expect(err).To(BeIdenticalTo(exhausted))
		Expect(retries()).To(BeEmpty())
	})

	It("does not try again once the run is cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sess.steps = []scriptedStep{cut}

		_, err := conv.Run(ctx, "")

		Expect(err).To(MatchError(llms.ErrStreamEnded))
		Expect(retries()).To(BeEmpty())
	})

	It("keeps an interrupted turn's text out of the reply that replaces it", func() {
		sess.steps = []scriptedStep{
			{
				stream: []llms.StreamingEvent{
					llms.StreamingEventMessageStart{},
					llms.StreamingEventThinking{Text: "let me think"},
					llms.StreamingEventTextChunk{Text: "partial "},
				},
				err: interrupted(),
			},
			{
				stream: []llms.StreamingEvent{
					llms.StreamingEventMessageStart{},
					llms.StreamingEventTextChunk{Text: "final"},
					llms.StreamingEventMessageEnd{Final: true},
				},
				done: true,
			},
		}

		_, err := conv.Run(context.Background(), "")
		Expect(err).NotTo(HaveOccurred())

		var texts []string
		var started []int
		for _, e := range events {
			switch e := e.(type) {
			case agent.ProgressTextMessage:
				texts = append(texts, e.Text)
			case agent.ProgressTurn:
				if e.Phase == agent.TurnStarted {
					started = append(started, e.Step)
				}
			}
		}
		Expect(texts).To(Equal([]string{"final"}))
		// The replacement is the same step, so it carries the same number.
		Expect(started).To(Equal([]int{1, 1}))
		Expect(retries()[0].Step).To(Equal(1))
	})
})
