package jobs

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ListCmd lists sessions newest-first, filtered server-side.
type ListCmd struct {
	connectFlags

	Project string            `help:"Only jobs for this project."`
	State   []string          `help:"Only jobs in these states (repeatable, comma-separated)." placeholder:"STATE"`
	Active  bool              `help:"Only jobs that have not finished."`
	Mode    string            `help:"Only jobs run in this agent mode."`
	PRRef   string            `help:"Only jobs working on this pull request." name:"pr-ref"`
	Since   time.Duration     `help:"Only jobs created within this window (e.g. 24h)."`
	Meta    map[string]string `help:"Only jobs annotated with this key=value (repeatable; all must match)." placeholder:"KEY=VALUE"`
	Limit   int               `help:"Maximum jobs to return." default:"50"`

	IncludePreviews bool `help:"Also list the boot sessions of preview environments."`

	All     bool              `help:"Follow pagination until every matching job has been listed."`
	JSON    bool              `help:"Emit JSON instead of a table." name:"json"`
}

func (c *ListCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	req := &v1.ListSessionsRequest{
		Project:    c.Project,
		States:     c.State,
		ActiveOnly: c.Active,
		Mode:       c.Mode,
		PrRef:      c.PRRef,
		Metadata:   c.Meta,
		Limit:      int32(c.Limit),

		IncludePreviews: c.IncludePreviews,
	}
	if c.Since > 0 {
		req.CreatedAfter = timestamppb.New(time.Now().Add(-c.Since))
	}

	var sessions []*v1.GetSessionResponse
	for {
		resp, err := oc.ListSessions(ctx, connect.NewRequest(req))
		if err != nil {
			return fmt.Errorf("list jobs: %w", err)
		}
		sessions = append(sessions, resp.Msg.Sessions...)

		// Without --all a page is the answer; the limit is what the caller asked
		// to see, not a page size to loop over.
		if !c.All || resp.Msg.NextPageCursor == "" {
			break
		}
		req.PageCursor = resp.Msg.NextPageCursor
	}

	if c.JSON {
		msgs := make([]proto.Message, 0, len(sessions))
		for _, s := range sessions {
			msgs = append(msgs, s)
		}
		return PrintJSONList(msgs)
	}

	if len(sessions) == 0 {
		fmt.Fprintln(os.Stdout, "No matching jobs")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tPROJECT\tSTATE\tMODE\tAGE\tCOST\tPR\tPROMPT")
	for _, s := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.SessionId, s.Project, s.State, Dash(s.Mode),
			FormatAge(s.CreatedAt.AsTime()), formatCost(s.Cost),
			Dash(s.PrRef), Summarize(s.Prompt, 48))
	}
	return tw.Flush()
}

// formatCost renders spend at cent resolution, which is the granularity worth
// scanning a column of. A job that has not billed yet reads as "-" rather than
// as $0.00, so "nothing spent" and "nothing recorded" stay distinguishable.
func formatCost(c *v1.CostReport) string {
	if c == nil || (c.TotalUsd == 0 && c.InputTokens == 0 && c.OutputTokens == 0) {
		return "-"
	}
	return fmt.Sprintf("$%.2f", c.TotalUsd)
}

// ShowCmd prints one session in full.
type ShowCmd struct {
	connectFlags

	SessionID string `arg:"" name:"session-id" help:"Session to show."`
	JSON      bool   `help:"Emit JSON instead of a field listing." name:"json"`
}

func (c *ShowCmd) Run() error {
	resp, err := c.client().GetSession(context.Background(),
		connect.NewRequest(&v1.GetSessionRequest{SessionId: c.SessionID}))
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	s := resp.Msg

	if c.JSON {
		return PrintJSON(s)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(label, value string) {
		if value != "" {
			fmt.Fprintf(tw, "%s:\t%s\n", label, value)
		}
	}
	row("Session", s.SessionId)
	row("Project", s.Project)
	row("State", s.State)
	row("Mode", s.Mode)
	row("Message", s.Message)
	row("Error", s.Error)
	row("Created", formatTime(s.CreatedAt))
	row("Updated", formatTime(s.UpdatedAt))
	row("Queued", formatTime(s.QueuedAt))
	row("Priority", fmt.Sprintf("%d", s.Priority))
	if s.Attempts > 0 {
		row("Attempts", fmt.Sprintf("%d", s.Attempts))
	}
	row("Pull request", s.PullRequestUrl)
	row("PR ref", s.PrRef)
	row("Head branch", s.HeadBranch)
	row("Base branch", s.BaseBranch)
	row("Parent session", s.ParentSessionId)
	if cost := s.Cost; cost != nil && cost.TotalUsd > 0 {
		row("Cost", fmt.Sprintf("$%.4f (%d in / %d out / %d cached tokens)",
			cost.TotalUsd, cost.InputTokens, cost.OutputTokens, cost.CachedTokens))
	}
	// One row per annotation, sorted, so a job's record keeping reads the same
	// way every time it is printed. Printed directly rather than through row so
	// a key stored with an empty value still shows up as stored.
	for _, k := range slices.Sorted(maps.Keys(s.Metadata)) {
		fmt.Fprintf(tw, "Meta %s:\t%s\n", k, Dash(s.Metadata[k]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if prompt := strings.TrimSpace(s.Prompt); prompt != "" {
		fmt.Fprintf(os.Stdout, "\nPrompt:\n%s\n", prompt)
	}
	return nil
}

func formatTime(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() {
		return ""
	}
	t := ts.AsTime().Local()
	return fmt.Sprintf("%s (%s ago)", t.Format(time.RFC3339), FormatAge(t))
}
