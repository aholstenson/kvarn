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
	Remove bool `help:"Forget the preview entirely, release its hostnames and drop its saved state."`
	// Stopping normally saves whatever the preview was holding, so the next boot
	// comes back to it. --no-state is for a preview whose contents are in the
	// way: a half-migrated database, a seed that went wrong.
	NoState bool `help:"Take the preview down without saving its state." name:"no-state"`
	JSON    bool `help:"Emit JSON instead of a summary." name:"json"`
}

func (c *DownCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.StopPreview(ctx, connect.NewRequest(&v1.StopPreviewRequest{
		Project:   c.Project,
		Ref:       c.Ref,
		Remove:    c.Remove,
		SkipState: c.NoState,
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
	p := resp.Msg.Preview
	switch {
	case p.GetStateError() != "":
		fmt.Fprintf(os.Stderr, "Stopped the preview of %s, but its state could not be saved: %s\n",
			c.Ref, p.GetStateError())
	case p.GetStateBytes() > 0:
		fmt.Fprintf(os.Stderr, "Stopped the preview of %s and saved %s of state; the next request starts it again.\n",
			c.Ref, formatBytes(p.GetStateBytes()))
	default:
		fmt.Fprintf(os.Stderr, "Stopped the preview of %s; the next request will start it again.\n", c.Ref)
	}
	return nil
}

// ResetCmd drops a preview's saved state without touching the preview itself.
type ResetCmd struct {
	connectFlags

	Project string `arg:"" help:"Project the branch belongs to."`
	Ref     string `arg:"" help:"Branch whose saved state to drop."`

	JSON bool `help:"Emit JSON instead of a summary." name:"json"`
}

func (c *ResetCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.ResetPreviewState(ctx, connect.NewRequest(&v1.ResetPreviewStateRequest{
		Project: c.Project,
		Ref:     c.Ref,
	}))
	if err != nil {
		return fmt.Errorf("reset preview state: %w", err)
	}
	if c.JSON {
		return printJSON(resp.Msg.Preview)
	}
	fmt.Fprintf(os.Stderr, "Dropped the saved state of %s; the next start comes up empty.\n", c.Ref)
	return nil
}

// PruneCmd runs the state sweep by hand.
type PruneCmd struct {
	connectFlags

	OlderThan string `help:"Drop archives untouched for longer than this (e.g. 720h). Empty uses the orchestrator's own retention." name:"older-than"`
	JSON      bool   `help:"Emit JSON instead of a summary." name:"json"`
}

func (c *PruneCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	resp, err := oc.PrunePreviewState(ctx, connect.NewRequest(&v1.PrunePreviewStateRequest{
		OlderThan: c.OlderThan,
	}))
	if err != nil {
		return fmt.Errorf("prune preview state: %w", err)
	}
	if c.JSON {
		return printJSON(resp.Msg)
	}
	if resp.Msg.Removed == 0 {
		fmt.Fprintln(os.Stderr, "No preview state was old enough to drop.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "Dropped %d saved preview state archives, freeing %s.\n",
		resp.Msg.Removed, formatBytes(resp.Msg.BytesFreed))
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
	fmt.Fprintln(w, "PROJECT\tREF\tSTATE\tURL\tSTARTED\tLAST REQUEST\tDATA")
	for _, p := range previews {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Project,
			p.Ref,
			p.State,
			dash(p.Url),
			formatAge(p.StartedAt),
			formatAge(p.LastRequestAt),
			formatSavedState(p),
		)
	}
	return w.Flush()
}

// formatSavedState renders what a preview is holding: its size and how long ago
// it was written, "failed" when the last capture did not finish, and "-" for a
// preview that keeps nothing.
func formatSavedState(p *v1.Preview) string {
	if p.GetStateError() != "" {
		return "failed"
	}
	if p.GetStateBytes() == 0 {
		return "-"
	}
	return fmt.Sprintf("%s (%s)", formatBytes(p.GetStateBytes()), formatAge(p.GetStateSavedAt()))
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
