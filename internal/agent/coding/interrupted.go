package coding

import (
	"context"
	"errors"
	"fmt"
	"time"

	llms "github.com/aholstenson/llms-go"
)

// interruptRetryBackoff is the pause before each new try of a step whose model
// call failed after part of its output had arrived. Its length also sets how
// many times the step is tried again. It is a variable so that tests can run
// without the pauses.
//
// llms-go retries a transient failure by itself only while no output has
// reached the caller, because a new attempt streams the turn again from the
// start. A failure after the first token, for example a stream that a proxy
// cut in the middle of the model's reasoning, comes back to the caller at
// once. Without this retry such a failure ends a job that can be hours into
// its work, although the conversation is intact.
var interruptRetryBackoff = []time.Duration{2 * time.Second, 10 * time.Second}

// interruptRecovery counts the interrupted tries of the current step. Its zero
// value is ready to use.
type interruptRecovery struct {
	consecutive int
}

// stepped records a step that got through, so the next interrupted step gets
// all its retries again.
func (r *interruptRecovery) stepped() {
	r.consecutive = 0
}

// recoverFromInterrupt handles an error from a session step. It returns nil
// when the step can be tried again: the model call failed with a transient
// error after part of its output had arrived, and retries are left. Any other
// error comes back unchanged. When the retries are used up, the error says
// how many times the step was interrupted. failedStep is the 1-based step that
// failed, for the error message.
//
// llms-go keeps the session usable after such a failure: the turn leaves no
// trace in the history and the step is not counted. The output that arrived
// was not recorded, so it is discarded here before the next try streams the
// turn from the start.
func (c *codingConversation) recoverFromInterrupt(ctx context.Context, r *interruptRecovery, failedStep int, err error) error {
	if !llms.IsUnavailable(err) || !errors.Is(err, llms.ErrStreamingPartialOutput) || ctx.Err() != nil {
		return err
	}

	r.consecutive++
	maxAttempts := len(interruptRetryBackoff) + 1
	if r.consecutive >= maxAttempts {
		return fmt.Errorf("the model call for step %d failed %d times after it had started to respond: %w",
			failedStep, r.consecutive, err)
	}

	agentID := streamAgentID(ctx)
	streamStep := c.abandonTurn(agentID)
	delay := interruptRetryBackoff[r.consecutive-1]

	var ue *llms.UnavailableError
	errors.As(err, &ue)
	c.reportRetry(agentID, streamStep, llms.RetryNotice{
		Attempt:     r.consecutive,
		MaxAttempts: maxAttempts,
		Delay:       delay,
		StatusCode:  ue.StatusCode,
		Provider:    ue.Provider,
		Model:       ue.Model,
		Err:         err,
	})

	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return err
	case <-timer.C:
		return nil
	}
}
