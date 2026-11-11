package coding

import (
	"context"
	"errors"
	"fmt"

	llms "github.com/aholstenson/llms-go"

	"github.com/aholstenson/kvarn/internal/agent"
)

// contentSteerPrompt is sent to an agent after its turn was blocked twice in a
// row by the model provider's content filter. A block leaves the conversation
// as it was, so without this message the agent does not know that a turn was
// lost, and the same transcript is likely to give the same output again.
//
// The message gives the agent no way out. An agent that stops early ends its
// turn with a text reply, and the run then validates and delivers work that is
// not finished, as a pull request or a comment. A run that cannot get past the
// block fails with a clear error instead.
const contentSteerPrompt = "Your last response was stopped by the model provider's content filter. " +
	"It was not recorded and none of it took effect. " +
	"Continue the task from where you were, with a different approach. " +
	"Do not repeat payloads, credentials or exploit strings in your replies or tool arguments when the task does not need them; refer to them by file and line. " +
	"Do not end the task early because of this message."

// maxContentSteers is how many steering messages one agent run may receive.
// A run that needs more is blocked by the work itself, and more messages only
// add cost before the same failure.
const maxContentSteers = 2

// blockTries is how many times a blocked step is tried when a steer is still
// available: the first try, one retry of the same transcript, and one try
// after the steer.
const blockTries = 3

// blockAction is what to do after a content policy block.
type blockAction int

const (
	// blockRetry tries the same transcript again. The provider's classifier
	// is not deterministic, so this clears some blocks.
	blockRetry blockAction = iota + 1
	// blockSteer tells the agent about the block before it tries again.
	blockSteer
	// blockGiveUp ends the run.
	blockGiveUp
)

// blockRecovery decides what one agent run does after a content policy block.
// Its zero value is ready to use.
type blockRecovery struct {
	// consecutive counts the blocks in a row on the current step.
	consecutive int
	// steered is set when the current step received a steering message.
	steered bool
	// steers counts the steering messages this run received.
	steers int
}

// next records a block and returns what to do about it.
func (r *blockRecovery) next() blockAction {
	r.consecutive++
	switch {
	case r.consecutive == 1:
		return blockRetry
	case r.consecutive == 2 && r.steers < maxContentSteers:
		r.steers++
		r.steered = true
		return blockSteer
	default:
		return blockGiveUp
	}
}

// stepped records a step that got through, so the next block starts the
// recovery again from a retry.
func (r *blockRecovery) stepped() {
	r.consecutive = 0
	r.steered = false
}

// injector is the part of a session that takes a message before the next
// step.
type injector interface {
	Inject(msgs ...*llms.Message)
}

// recoverFromBlock handles an error from a session step. It returns nil when
// the step can be tried again. Any other error, and a block that the recovery
// gives up on, comes back as the error that ends the run. blockedStep is the
// 1-based step that failed, for the error message.
//
// llms-go keeps a session usable after a block: the turn leaves no trace in
// the history and the step is not counted. That is what lets the same session
// try again, with or without a steering message.
func (c *codingConversation) recoverFromBlock(ctx context.Context, r *blockRecovery, sess injector, blockedStep int, err error) error {
	var policy *llms.ContentPolicyError
	if !errors.As(err, &policy) || ctx.Err() != nil {
		return err
	}

	agentID := streamAgentID(ctx)
	streamStep := c.abandonTurn(agentID)
	action := r.next()
	if action == blockGiveUp {
		return contentBlockedError(blockedStep, r.consecutive, r.steered, policy)
	}

	maxAttempts := blockTries
	if r.steers >= maxContentSteers && action == blockRetry {
		maxAttempts = 2
	}
	note := policy.Error() + "; trying again"
	if action == blockSteer {
		note = policy.Error() + "; asking the agent to take a different approach"
		sess.Inject(llms.NewMessage(llms.RoleUser, llms.NewTextPart(contentSteerPrompt)))
	}
	c.reportRetry(agentID, streamStep, llms.RetryNotice{
		Attempt:     r.consecutive,
		MaxAttempts: maxAttempts,
		StatusCode:  policy.StatusCode,
		Provider:    policy.Provider,
		Model:       policy.Model,
		Err:         errors.New(note),
	})
	return nil
}

// abandonTurn forgets the model call that a block stopped and returns its
// step number. The call emitted a turn start but will never emit an end, and
// any text it streamed was not recorded, so that text must not open the reply
// of the call that replaces it. The step number is given back, because the
// call that replaces it is the same step.
func (c *codingConversation) abandonTurn(agentID string) int {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if buf := c.textBufs[agentID]; buf != nil {
		buf.Reset()
	}
	t := c.turn(agentID)
	step := t.step
	if t.step > 0 {
		t.step--
	}
	t.responded = false
	return step
}

// contentBlockedError is the error a run ends with when the content filter
// keeps blocking it.
func contentBlockedError(step, tries int, steered bool, policy *llms.ContentPolicyError) error {
	how := "also after a retry"
	if steered {
		how = "also after a retry and a request to take a different approach"
	}
	return fmt.Errorf("%w: the model provider's content filter blocked step %d %d times, %s: %w",
		agent.ErrRefused, step, tries, how, policy)
}

// refusalError is the error a run ends with when the model wrote a refusal.
func refusalError(err error) error {
	return fmt.Errorf("%w: the model wrote a refusal: %w", agent.ErrRefused, err)
}
