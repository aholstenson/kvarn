// Package queue implements the `kvarn queue` CLI: what the orchestrator is
// holding and in what order it will run it. It answers the question a job
// listing cannot — whether a job is waiting because the host is full or merely
// because others are ahead of it.
package queue

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"github.com/aholstenson/kvarn/internal/cmd/jobs"
)

// Cmd is the parent command for `kvarn queue <subcommand>`.
type Cmd struct {
	Status StatusCmd `cmd:"" help:"Show backlog depth, pipeline population and free capacity."`
	List   ListCmd   `cmd:"" help:"List the backlog in the order it will be dispatched."`
}

type connectFlags struct {
	Addr   string `help:"Orchestrator address." default:"http://localhost:8080"`
	APIKey string `help:"API key for the orchestrator." env:"KVARN_API_KEY" default:""`
}

func (c connectFlags) client() kvarnv1connect.OrchestratorServiceClient {
	return client.NewOrchestrator(c.Addr, c.APIKey)
}

// StatusCmd reports how full the host is.
type StatusCmd struct {
	connectFlags

	JSON bool `help:"Emit JSON instead of a summary." name:"json"`
}

func (c *StatusCmd) Run() error {
	resp, err := c.client().GetQueueStats(context.Background(),
		connect.NewRequest(&v1.GetQueueStatsRequest{}))
	if err != nil {
		return fmt.Errorf("queue stats: %w", err)
	}
	st := resp.Msg

	if c.JSON {
		return jobs.PrintJSON(st)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Backlog:\t%s\n", formatBound(st.Backlog, st.MaxBacklog))
	fmt.Fprintf(tw, "Pipeline:\t%s\n", formatBound(st.Dispatched, st.MaxDispatched))
	fmt.Fprintf(tw, "Awaiting capacity:\t%d\n", st.AdmissionQueue)

	// An unbounded scheduler reports a zero pool; saying so beats printing
	// "0 / 0" as if the host had no resources.
	if total := add(st.Used, st.Free); total.GetCpuMillis() == 0 && total.GetMemoryBytes() == 0 {
		fmt.Fprintf(tw, "Capacity:\tunbounded (no admission control)\n")
	} else {
		fmt.Fprintf(tw, "CPU:\t%d / %d mcpu\n", st.Used.GetCpuMillis(), total.GetCpuMillis())
		fmt.Fprintf(tw, "Memory:\t%s / %s\n", formatBytes(st.Used.GetMemoryBytes()), formatBytes(total.GetMemoryBytes()))
		fmt.Fprintf(tw, "Disk:\t%s / %s\n", formatBytes(st.Used.GetDiskBytes()), formatBytes(total.GetDiskBytes()))
	}

	if st.DiskMeasured {
		gate := "admitting"
		if !st.DiskGateOpen {
			gate = "HELD — free space below floor"
		}
		fmt.Fprintf(tw, "Host disk:\t%s free, floor %s (%s)\n",
			formatBytes(st.DiskAvailableBytes), formatBytes(st.DiskFloorBytes), gate)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(st.PerProject) == 0 {
		return nil
	}
	fmt.Fprintln(os.Stdout)
	pw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(pw, "PROJECT\tBACKLOG\tIN PIPELINE")
	for _, p := range st.PerProject {
		fmt.Fprintf(pw, "%s\t%d\t%d\n", p.Project, p.Backlog, p.Dispatched)
	}
	return pw.Flush()
}

// ListCmd prints the backlog in dispatch order.
type ListCmd struct {
	connectFlags

	Project string `help:"Only entries for this project. Positions stay relative to the whole backlog."`
	Limit   int    `help:"Maximum entries to return." default:"50"`
	JSON    bool   `help:"Emit JSON instead of a table." name:"json"`
}

func (c *ListCmd) Run() error {
	resp, err := c.client().ListQueue(context.Background(),
		connect.NewRequest(&v1.ListQueueRequest{
			Project: c.Project,
			Limit:   int32(c.Limit),
		}))
	if err != nil {
		return fmt.Errorf("list queue: %w", err)
	}

	if c.JSON {
		return jobs.PrintJSON(resp.Msg)
	}

	if len(resp.Msg.Entries) == 0 {
		fmt.Fprintln(os.Stdout, "Backlog is empty")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSESSION\tPROJECT\tMODE\tPRIO\tEFFECTIVE\tWAITING\tTRIES\tPROMPT")
	for _, e := range resp.Msg.Entries {
		s := e.Session
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%d\t%s\t%d\t%s\n",
			e.Position, s.SessionId, s.Project, jobs.Dash(s.Mode),
			s.Priority, e.EffectivePriority, jobs.FormatAge(s.QueuedAt.AsTime()),
			s.Attempts, jobs.Summarize(s.Prompt, 40))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if int(resp.Msg.Backlog) > len(resp.Msg.Entries) {
		fmt.Fprintf(os.Stdout, "\nShowing %d of %d waiting\n", len(resp.Msg.Entries), resp.Msg.Backlog)
	}
	return nil
}

// formatBound renders a count against its configured limit, or on its own when
// the limit is unbounded.
func formatBound(n, max int32) string {
	if max <= 0 {
		return fmt.Sprintf("%d (unbounded)", n)
	}
	return fmt.Sprintf("%d / %d", n, max)
}

func add(a, b *v1.Capacity) *v1.Capacity {
	return &v1.Capacity{
		CpuMillis:   a.GetCpuMillis() + b.GetCpuMillis(),
		MemoryBytes: a.GetMemoryBytes() + b.GetMemoryBytes(),
		DiskBytes:   a.GetDiskBytes() + b.GetDiskBytes(),
	}
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
