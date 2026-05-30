package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/aholstenson/kvarn/internal/runner"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	// `kvarn connect` is the in-VM entrypoint invoked by the systemd unit.
	// It takes no arguments — token and transport are loaded from the env
	// file systemd has already merged into our environment, so secrets
	// never appear on /proc/<pid>/cmdline.
	if len(os.Args) > 1 && os.Args[1] == "connect" {
		if err := runner.Connect(); err != nil {
			slog.Error("runner connect failed", "error", err)
			os.Exit(1)
		}
		return
	}

	var cmd runner.Cmd
	flag.StringVar(&cmd.Addr, "addr", ":9090", "Address to listen on")
	flag.Parse()

	if err := cmd.Run(); err != nil {
		slog.Error("runner failed", "error", err)
		os.Exit(1)
	}
}
