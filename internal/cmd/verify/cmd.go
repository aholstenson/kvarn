package verify

import (
	"context"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
)

const verifyToken = "kvarn-verify-ok"

type Cmd struct {
	Addr    string `help:"Orchestrator address." default:"http://localhost:8080"`
	Command string `help:"Command to run instead of the default echo check." optional:""`
}

func (c *Cmd) Run() error {
	command := "sh"
	args := []string{"-c", c.Command}
	if c.Command == "" {
		command = "echo"
		args = []string{verifyToken}
	}

	displayCmd := command
	if c.Command != "" {
		displayCmd = c.Command
	}
	fmt.Printf("Verifying runner on VM (command: %s)...\n", displayCmd)
	client := kvarnv1connect.NewOrchestratorServiceClient(http.DefaultClient, c.Addr)

	resp, err := client.ExecuteJob(context.Background(), connect.NewRequest(&v1.ExecuteJobRequest{
		Command: command,
		Args:    args,
	}))
	if err != nil {
		return fmt.Errorf("verify failed: %w", err)
	}

	if resp.Msg.ExitCode != 0 {
		return fmt.Errorf("verify failed: exit code %d\nstderr: %s", resp.Msg.ExitCode, resp.Msg.Stderr)
	}

	fmt.Printf("OK: command executed successfully on VM %s (exit code %d)\n", resp.Msg.VmId, resp.Msg.ExitCode)

	if c.Command != "" {
		if resp.Msg.Stdout != "" {
			fmt.Printf("\nstdout:\n%s", resp.Msg.Stdout)
		}
		if resp.Msg.Stderr != "" {
			fmt.Printf("\nstderr:\n%s", resp.Msg.Stderr)
		}
	}

	if vi := resp.Msg.VmInfo; vi != nil {
		fmt.Printf("\nVM stats:\n")
		if vi.CpuCount > 0 {
			fmt.Printf("  CPU:    %d cores", vi.CpuCount)
			if vi.CpuModel != "" {
				fmt.Printf(" (%s)", vi.CpuModel)
			}
			fmt.Println()
		}
		if vi.MemTotalMb > 0 {
			fmt.Printf("  Memory: %d MB available / %d MB total\n", vi.MemAvailableMb, vi.MemTotalMb)
		}
		if vi.DiskTotalMb > 0 {
			fmt.Printf("  Disk:   %d MB used / %d MB total\n", vi.DiskUsedMb, vi.DiskTotalMb)
		}
	}

	return nil
}
