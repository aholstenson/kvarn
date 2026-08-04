// Package jobs implements the `kvarn jobs` CLI: listing, inspecting and
// managing the runs the orchestrator holds. Everything here talks to a running
// orchestrator over the OrchestratorService — the session store is
// orchestrator-owned, so unlike `kvarn repo` these commands have nothing to
// read on their own.
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Cmd is the parent command for `kvarn jobs <subcommand>`.
type Cmd struct {
	Start    StartCmd    `cmd:"" help:"Start a project-aware job."`
	List     ListCmd     `cmd:"" help:"List jobs, newest first."`
	Show     ShowCmd     `cmd:"" help:"Show one job in full."`
	Watch    WatchCmd    `cmd:"" help:"Stream a job's events until it finishes."`
	Events   EventsCmd   `cmd:"" help:"Replay a job's recorded event history."`
	Cancel   CancelCmd   `cmd:"" help:"Cancel one job, or every job matching a filter."`
	Retry    RetryCmd    `cmd:"" help:"Resubmit a finished job as a new one."`
	Priority PriorityCmd `cmd:"" help:"Change the backlog priority of a job waiting to start."`
}

// connectFlags are what every subcommand needs to reach the orchestrator.
// Embedded rather than declared on the parent so each subcommand's help lists
// them, matching the rest of the CLI.
type connectFlags struct {
	Addr   string `help:"Orchestrator address. Empty = the host-local socket if present, else http://localhost:8080." env:"KVARN_ADDR" default:""`
	APIKey string `help:"API key for the orchestrator." env:"KVARN_API_KEY" default:""`
}

func (c connectFlags) client() kvarnv1connect.OrchestratorServiceClient {
	return client.NewOrchestrator(client.Resolve(c.Addr), c.APIKey)
}

// PrintJSON writes a protobuf message as indented JSON. Proto field names are
// kept, and defaults are emitted rather than elided, so a script sees the same
// keys whether or not a job has reached the stage that fills them in.
func PrintJSON(m proto.Message) error {
	b, err := protojson.MarshalOptions{
		Multiline:         true,
		Indent:            "  ",
		UseProtoNames:     true,
		EmitDefaultValues: true,
	}.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	// protojson emits no trailing newline.
	fmt.Fprintln(os.Stdout, string(b))
	return nil
}

// PrintJSONList writes a heterogeneous list of messages as a JSON array. It
// exists because protojson marshals a message, not a slice, and a listing
// assembled across several pages has no single message to stand for it.
func PrintJSONList(messages []proto.Message) error {
	opts := protojson.MarshalOptions{UseProtoNames: true, EmitDefaultValues: true}
	raw := make([]json.RawMessage, 0, len(messages))
	for _, m := range messages {
		b, err := opts.Marshal(m)
		if err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
		raw = append(raw, b)
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(b))
	return nil
}

// FormatAge renders a duration as a compact age ("4m", "3h12m", "2d"). An
// unset timestamp reads as "-" rather than as 56 years.
func FormatAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// Summarize collapses a prompt to one short line, so a listing stays one row
// per job whatever the prompt contains.
func Summarize(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}

// Dash renders an empty optional field as "-" so columns stay aligned.
func Dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
