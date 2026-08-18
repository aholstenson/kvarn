package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// UpCmd starts a preview environment for a branch.
type UpCmd struct {
	connectFlags

	Project string `arg:"" help:"Project the branch belongs to."`
	Ref     string `arg:"" help:"Branch to preview."`

	Wait bool `help:"Wait for the preview to be serving before returning." default:"true" negatable:""`
	JSON bool `help:"Emit JSON instead of a summary." name:"json"`
}

func (c *UpCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	// A first boot clones, installs dependencies and runs setup, so it takes a
	// minute or more. Following the state stream rather than blocking on the
	// call is what turns that into something legible: the same phases the
	// holding page shows, printed as they happen.
	resp, err := oc.StartPreview(ctx, connect.NewRequest(&v1.StartPreviewRequest{
		Project: c.Project,
		Ref:     c.Ref,
	}))
	if err != nil {
		return fmt.Errorf("start preview: %w", err)
	}
	p := resp.Msg.Preview

	if c.Wait && p.State == "booting" {
		p, err = c.follow(ctx, p)
		if err != nil {
			return err
		}
	}

	if c.JSON {
		return printJSON(p)
	}
	return report(p)
}

// follow streams the boot's phases to stderr and returns the preview's settled
// state. Progress goes to stderr so the URL on stdout stays pipeable.
func (c *UpCmd) follow(ctx context.Context, p *v1.Preview) (*v1.Preview, error) {
	oc := c.client()
	stream, err := oc.WatchPreview(ctx, connect.NewRequest(&v1.WatchPreviewRequest{
		Project: c.Project,
		Ref:     c.Ref,
	}))
	if err != nil {
		return nil, fmt.Errorf("watch preview: %w", err)
	}
	defer stream.Close()

	latest := p
	var lastPhase string
	for stream.Receive() {
		update := stream.Msg()
		if update.Preview != nil {
			latest = update.Preview
		}
		if update.Phase != "" && update.Phase != lastPhase {
			fmt.Fprintf(os.Stderr, "  %s\n", update.Phase)
			lastPhase = update.Phase
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("watch preview: %w", err)
	}
	return latest, nil
}

// report prints what a person asking for a preview wants to know: whether it is
// up, and where.
func report(p *v1.Preview) error {
	switch p.State {
	case "running":
		fmt.Fprintf(os.Stderr, "Preview of %s is running.\n", p.Ref)
		for _, site := range p.Sites {
			fmt.Fprintf(os.Stderr, "  %-12s ", site.Name)
			fmt.Fprintln(os.Stdout, site.Url)
		}
		if len(p.Sites) == 0 {
			fmt.Fprintln(os.Stdout, p.Url)
		}
		return nil
	case "failed":
		return fmt.Errorf("preview of %s failed to start: %s", p.Ref, dash(p.Error))
	case "booting":
		fmt.Fprintf(os.Stderr, "Preview of %s is starting; follow it with kvarn jobs watch %s\n", p.Ref, p.SessionId)
		return nil
	default:
		fmt.Fprintf(os.Stderr, "Preview of %s is %s.\n", p.Ref, p.State)
		return nil
	}
}
