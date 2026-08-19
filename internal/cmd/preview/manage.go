package preview

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// DownCmd stops a preview environment.
type DownCmd struct {
	connectFlags

	Project string `arg:"" help:"Project the branch belongs to."`
	Ref     string `arg:"" help:"Branch whose preview to stop."`

	// Stopping leaves the record and its hostnames in place, so the next
	// request boots the preview again. --remove is for when that is not wanted:
	// the branch is gone, or the name should be freed.
	Remove bool `help:"Forget the preview entirely and release its hostnames."`
	JSON   bool `help:"Emit JSON instead of a summary." name:"json"`
}

func (c *DownCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.StopPreview(ctx, connect.NewRequest(&v1.StopPreviewRequest{
		Project: c.Project,
		Ref:     c.Ref,
		Remove:  c.Remove,
	}))
	if err != nil {
		return fmt.Errorf("stop preview: %w", err)
	}

	if c.JSON {
		return printJSON(resp.Msg.Preview)
	}
	if c.Remove {
		fmt.Fprintf(os.Stderr, "Removed the preview of %s.\n", c.Ref)
		return nil
	}
	fmt.Fprintf(os.Stderr, "Stopped the preview of %s; the next request will start it again.\n", c.Ref)
	return nil
}

// ListCmd lists preview environments.
type ListCmd struct {
	connectFlags

	Project string `help:"Only previews for this project."`
	JSON    bool   `help:"Emit JSON instead of a table." name:"json"`
}

func (c *ListCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.ListPreviews(ctx, connect.NewRequest(&v1.ListPreviewsRequest{
		Project: c.Project,
	}))
	if err != nil {
		return fmt.Errorf("list previews: %w", err)
	}

	previews := resp.Msg.Previews
	sort.Slice(previews, func(i, j int) bool { return previews[i].Id < previews[j].Id })

	if c.JSON {
		return printJSON(resp.Msg)
	}

	if len(previews) == 0 {
		fmt.Fprintln(os.Stderr, "No preview environments.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tREF\tSTATE\tURL\tSTARTED\tLAST REQUEST")
	for _, p := range previews {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Project,
			p.Ref,
			p.State,
			dash(p.Url),
			formatAge(p.StartedAt),
			formatAge(p.LastRequestAt),
		)
	}
	return w.Flush()
}

// LogsCmd prints the recent output of a preview's services.
type LogsCmd struct {
	connectFlags

	Project string `arg:"" help:"Project the branch belongs to."`
	Ref     string `arg:"" help:"Branch whose preview to read."`

	JSON bool `help:"Emit JSON instead of raw output." name:"json"`
}

func (c *LogsCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.GetPreview(ctx, connect.NewRequest(&v1.GetPreviewRequest{
		Project: c.Project,
		Ref:     c.Ref,
	}))
	if err != nil {
		return fmt.Errorf("get preview: %w", err)
	}

	if c.JSON {
		return printJSON(resp.Msg)
	}

	// The buffer holds a bounded tail, so an empty one is a preview that has
	// not started anything yet rather than a preview that printed nothing.
	logs := resp.Msg.Logs
	if strings.TrimSpace(logs) == "" {
		fmt.Fprintf(os.Stderr, "No output retained for the preview of %s (state: %s).\n",
			c.Ref, resp.Msg.Preview.GetState())
		return nil
	}
	fmt.Fprintln(os.Stdout, strings.TrimRight(logs, "\n"))
	return nil
}
