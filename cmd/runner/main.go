package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/aholstenson/kvarn/internal/runner"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	var cmd runner.Cmd
	flag.StringVar(&cmd.Addr, "addr", ":9090", "Address to listen on")
	flag.StringVar(&cmd.OrchestratorAddr, "orchestrator-addr", "", "Orchestrator address (connect mode)")
	flag.StringVar(&cmd.Token, "token", "", "Bootstrap token for orchestrator registration")

	var vsockPort uint
	flag.UintVar(&vsockPort, "vsock-port", 0, "Vsock port to connect to orchestrator")
	flag.Parse()

	cmd.VsockPort = uint32(vsockPort)

	if err := cmd.Run(); err != nil {
		slog.Error("runner failed", "error", err)
		os.Exit(1)
	}
}
