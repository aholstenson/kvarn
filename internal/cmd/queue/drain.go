package queue

import (
	"context"
	"fmt"
	"os"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// DrainCmd takes the orchestrator out of service without killing what it is
// already doing.
type DrainCmd struct {
	connectFlags

	Reason string `help:"Why the host is being drained; reported by 'kvarn queue status'." default:""`
	Wait   bool   `help:"Block until the pipeline is empty and the host is safe to stop."`
	// A drain is usually the first half of "stop the service", so a wait that
	// could hang forever would hang a deploy. The bound turns that into a
	// non-zero exit the surrounding script can act on.
	Timeout  time.Duration `help:"Give up waiting after this long and exit non-zero." default:"30m"`
	Interval time.Duration `help:"How often to poll while waiting." default:"5s"`
}

func (c *DrainCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.SetDrain(ctx, connect.NewRequest(&v1.SetDrainRequest{
		Draining: true,
		Reason:   c.Reason,
	}))
	if err != nil {
		return fmt.Errorf("drain: %w", err)
	}

	if resp.Msg.PreviouslyDraining {
		fmt.Fprintln(os.Stdout, "Already draining.")
	} else {
		fmt.Fprintln(os.Stdout, "Draining: no further jobs will start.")
	}
	if n := len(resp.Msg.Requeued); n > 0 {
		fmt.Fprintf(os.Stdout, "Returned %d job(s) to the backlog; they had not started running.\n", n)
	}
	fmt.Fprintf(os.Stdout, "%d job(s) still running.\n", resp.Msg.Dispatched)

	if !c.Wait || resp.Msg.Dispatched == 0 {
		if resp.Msg.Dispatched == 0 {
			fmt.Fprintln(os.Stdout, "Host is idle and safe to stop.")
		}
		return nil
	}
	return c.waitForIdle(ctx)
}

// waitForIdle polls until nothing is left in the pipeline. It polls rather than
// streams because the answer is one number and the wait is measured in the
// runtime of a job, so a dropped connection mid-deploy should cost one tick
// rather than the whole wait.
func (c *DrainCmd) waitForIdle(ctx context.Context) error {
	oc := c.client()
	deadline := time.Now().Add(c.Timeout)
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()

	last := int32(-1)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		resp, err := oc.GetQueueStats(ctx, connect.NewRequest(&v1.GetQueueStatsRequest{}))
		if err != nil {
			// A host that has already been stopped is the successful end of
			// this wait, not a failure, but the two are indistinguishable from
			// here — report and keep trying until the deadline.
			fmt.Fprintf(os.Stderr, "queue stats: %v\n", err)
		} else {
			if !resp.Msg.Draining {
				return fmt.Errorf("the orchestrator is no longer draining; something resumed it")
			}
			if n := resp.Msg.Dispatched; n != last {
				last = n
				fmt.Fprintf(os.Stdout, "%d job(s) still running.\n", n)
			}
			if resp.Msg.Dispatched == 0 {
				fmt.Fprintln(os.Stdout, "Host is idle and safe to stop.")
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("still not idle after %s; %d job(s) running", c.Timeout, last)
		}
	}
}

// ResumeCmd puts a drained orchestrator back into service.
type ResumeCmd struct {
	connectFlags
}

func (c *ResumeCmd) Run() error {
	resp, err := c.client().SetDrain(context.Background(), connect.NewRequest(&v1.SetDrainRequest{
		Draining: false,
	}))
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	if !resp.Msg.PreviouslyDraining {
		fmt.Fprintln(os.Stdout, "Not draining; nothing to resume.")
		return nil
	}
	fmt.Fprintln(os.Stdout, "Resumed: the backlog will be dispatched again.")
	return nil
}
