package coding

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
)

// scriptedStep is one call to Step on a scriptedSession: the streaming events
// it emits, then its outcome.
type scriptedStep struct {
	stream []llms.StreamingEvent
	err    error
	done   bool
}

// scriptedSession plays back a fixed list of steps. Like a real session, a
// failed step reports only the steps that completed before it.
type scriptedSession struct {
	conv      *codingConversation
	steps     []scriptedStep
	completed int
	injected  []*llms.Message
	result    llms.Result
	resultErr error
}

func (s *scriptedSession) Step(ctx context.Context) (llms.StepInfo, bool, error) {
	Expect(s.steps).NotTo(BeEmpty(), "Step was called more often than the script allows")
	step := s.steps[0]
	s.steps = s.steps[1:]
	for _, e := range step.stream {
		Expect(s.conv.handleStreamingEvent(ctx, e)).To(Succeed())
	}
	if step.err != nil {
		return llms.StepInfo{Step: s.completed}, false, step.err
	}
	s.completed++
	return llms.StepInfo{Step: s.completed}, step.done, nil
}

func (s *scriptedSession) Result() (llms.Result, error) {
	if s.resultErr != nil {
		return nil, s.resultErr
	}
	if s.result != nil {
		return s.result, nil
	}
	return llms.TextResult{Text: "done"}, nil
}

func (s *scriptedSession) Messages() []*llms.Message { return s.injected }

func (s *scriptedSession) Inject(msgs ...*llms.Message) {
	s.injected = append(s.injected, msgs...)
}

var _ = Describe("content policy blocks", func() {
	var (
		conv   *codingConversation
		sess   *scriptedSession
		events []agent.ProgressEvent
	)

	policy := func() error {
		return &llms.ContentPolicyError{Provider: "anthropic", Model: "claude-test", Code: "refusal"}
	}
	blocked := scriptedStep{err: policy()}
	ok := scriptedStep{}
	finish := scriptedStep{done: true}

	BeforeEach(func() {
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

	It("tries a blocked step again without telling the agent", func() {
		sess.steps = []scriptedStep{blocked, finish}

		text, err := conv.Run(context.Background(), "")

		Expect(err).NotTo(HaveOccurred())
		Expect(text).To(Equal("done"))
		Expect(sess.injected).To(BeEmpty())
		Expect(retries()).To(HaveLen(1))
		Expect(retries()[0].Attempt).To(Equal(1))
		Expect(retries()[0].MaxAttempts).To(Equal(3))
		Expect(retries()[0].Provider).To(Equal("anthropic"))
		Expect(retries()[0].Error).To(ContainSubstring("trying again"))
	})

	It("tells the agent about the block when the retry is blocked too", func() {
		sess.steps = []scriptedStep{blocked, blocked, finish}

		_, err := conv.Run(context.Background(), "")

		Expect(err).NotTo(HaveOccurred())
		Expect(sess.injected).To(HaveLen(1))
		steer := sess.injected[0]
		Expect(steer.Role).To(Equal(llms.RoleUser))
		Expect(steer.Parts[0].(*llms.TextPart).Text).To(Equal(contentSteerPrompt))
		Expect(retries()[1].Error).To(ContainSubstring("different approach"))
	})

	It("does not invite the agent to stop, because a stopped agent delivers unfinished work", func() {
		Expect(contentSteerPrompt).To(ContainSubstring("Do not end the task early"))
		Expect(strings.ToLower(contentSteerPrompt)).NotTo(ContainSubstring("stop and"))
	})

	It("fails with a clear error when the steer does not clear the block", func() {
		sess.steps = []scriptedStep{ok, ok, ok, ok, blocked, blocked, blocked}

		_, err := conv.Run(context.Background(), "")

		Expect(err).To(MatchError(agent.ErrRefused))
		var pe *llms.ContentPolicyError
		Expect(errors.As(err, &pe)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("blocked step 5 3 times"))
		Expect(err.Error()).To(ContainSubstring("different approach"))
		Expect(err.Error()).To(ContainSubstring("anthropic/claude-test"))
	})

	It("recognizes a block that had already streamed output", func() {
		partial := scriptedStep{err: errors.Join(policy(), llms.ErrStreamingPartialOutput)}
		sess.steps = []scriptedStep{partial, finish}

		_, err := conv.Run(context.Background(), "")

		Expect(err).NotTo(HaveOccurred())
		Expect(retries()).To(HaveLen(1))
	})

	It("starts again from a retry once a step gets through", func() {
		sess.steps = []scriptedStep{blocked, blocked, ok, blocked, finish}

		_, err := conv.Run(context.Background(), "")

		Expect(err).NotTo(HaveOccurred())
		Expect(sess.injected).To(HaveLen(1))
	})

	It("stops steering once the job has used its steers", func() {
		conv.blocks.steers = maxContentSteers
		sess.steps = []scriptedStep{blocked, blocked}

		_, err := conv.Run(context.Background(), "")

		Expect(err).To(MatchError(agent.ErrRefused))
		Expect(err.Error()).To(ContainSubstring("2 times, also after a retry:"))
		Expect(sess.injected).To(BeEmpty())
		Expect(retries()[0].MaxAttempts).To(Equal(2))
	})

	It("keeps the steer limit across validation passes", func() {
		sess.steps = []scriptedStep{blocked, blocked, finish}
		_, err := conv.Run(context.Background(), "")
		Expect(err).NotTo(HaveOccurred())

		Expect(conv.blocks.steers).To(Equal(1))
		Expect(conv.blocks.consecutive).To(BeZero())
	})

	It("passes other errors through unchanged", func() {
		boom := errors.New("boom")
		sess.steps = []scriptedStep{{err: boom}}

		_, err := conv.Run(context.Background(), "")

		Expect(err).To(BeIdenticalTo(boom))
		Expect(retries()).To(BeEmpty())
	})

	It("does not try again once the run is cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		sess.steps = []scriptedStep{blocked}

		_, err := conv.Run(ctx, "")

		Expect(err).NotTo(MatchError(agent.ErrRefused))
		Expect(llms.IsContentPolicy(err)).To(BeTrue())
	})

	It("keeps a blocked turn's text out of the reply that replaces it", func() {
		sess.steps = []scriptedStep{
			{
				stream: []llms.StreamingEvent{
					llms.StreamingEventMessageStart{},
					llms.StreamingEventTextChunk{Text: "partial "},
				},
				err: policy(),
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

	It("reports a refusal the model wrote as a refusal", func() {
		sess.steps = []scriptedStep{finish}
		sess.resultErr = &llms.RefusalError{}

		_, err := conv.Run(context.Background(), "")

		Expect(err).To(MatchError(agent.ErrRefused))
		var re *llms.RefusalError
		Expect(errors.As(err, &re)).To(BeTrue())
	})
})
