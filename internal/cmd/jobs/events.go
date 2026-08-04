package jobs

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"google.golang.org/protobuf/proto"
)

// WatchCmd follows a job's event stream to its terminal state.
type WatchCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" help:"Session to watch."`
	From      int64  `help:"Resume after this event sequence; 0 replays the full history first." default:"0"`
}

func (c *WatchCmd) Run() error {
	return client.WatchSessionFrom(context.Background(), c.client(), c.SessionID, c.From)
}

// EventsCmd prints the durable event history of a job. Unlike watch it returns
// as soon as the recorded history runs out, which is what makes it usable on a
// finished job and in a script.
type EventsCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" help:"Session whose history to print."`
	After     int64  `help:"Only events after this sequence number." default:"0"`
	Limit     int    `help:"Maximum events to return; 0 lets the server decide." default:"0"`
	JSON      bool   `help:"Emit JSON instead of rendered lines." name:"json"`
}

func (c *EventsCmd) Run() error {
	resp, err := c.client().ListSessionEvents(context.Background(),
		connect.NewRequest(&v1.ListSessionEventsRequest{
			SessionId:     c.SessionID,
			AfterSequence: c.After,
			Limit:         int32(c.Limit),
		}))
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}

	if c.JSON {
		msgs := make([]proto.Message, 0, len(resp.Msg.Events))
		for _, e := range resp.Msg.Events {
			msgs = append(msgs, e)
		}
		return PrintJSONList(msgs)
	}

	for _, ev := range resp.Msg.Events {
		client.PrintUpdate(ev)
	}
	if len(resp.Msg.Events) == 0 {
		fmt.Fprintln(os.Stdout, "No recorded events")
	}
	return nil
}
