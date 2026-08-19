// Package preview implements the `kvarn preview` CLI: bringing preview
// environments up, taking them down, and finding out what they are doing.
//
// Like `kvarn jobs`, everything here talks to a running orchestrator over the
// OrchestratorService. A preview is a VM inside that process, reachable only
// through the network it owns, so there is nothing a local command could
// inspect on its own.
package preview

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cmd is the parent command for `kvarn preview <subcommand>`.
type Cmd struct {
	Up    UpCmd    `cmd:"" help:"Start a preview environment for a branch."`
	Down  DownCmd  `cmd:"" help:"Stop a preview environment."`
	List  ListCmd  `cmd:"" help:"List preview environments."`
	Logs  LogsCmd  `cmd:"" help:"Print the recent output of a preview's services."`
	Reset ResetCmd `cmd:"" help:"Drop a preview's saved state, so the next start comes up empty."`
	Prune PruneCmd `cmd:"" help:"Drop saved preview state nothing has come back to."`
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

// printJSON writes a protobuf message as indented JSON, with proto field names
// and defaults emitted, so a script sees the same keys whatever state a preview
// is in.
func printJSON(m proto.Message) error {
	b, err := protojson.MarshalOptions{
		Multiline:         true,
		Indent:            "  ",
		UseProtoNames:     true,
		EmitDefaultValues: true,
	}.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	fmt.Fprintln(os.Stdout, string(b))
	return nil
}

// formatAge renders a timestamp as a compact age ("4m", "3h12m", "2d"). A
// preview that is stopped or failed carries no start time at all, so an unset
// timestamp reads as "-" rather than as the age of the unix epoch.
func formatAge(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() {
		return "-"
	}
	t := ts.AsTime()
	if t.IsZero() || t.Unix() == 0 {
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

// formatBytes renders a saved-state size in the units a person reads.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// dash renders an empty optional field as "-" so columns stay aligned.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
