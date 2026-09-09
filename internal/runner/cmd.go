package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"syscall"
)

// Env var names. The token must never be passed on argv because /proc/<pid>/cmdline
// is world-readable on stock Linux — any job step running as the kvarn user could
// read it and impersonate the runner.
const (
	envBridgeToken     = "KVARN_BRIDGE_TOKEN"
	envBridgeVsockPort = "KVARN_BRIDGE_VSOCK_PORT"
	envBridgeAddr      = "KVARN_BRIDGE_ADDR"
	// envFilePath is unlinked after the runner has loaded its env so the
	// secret doesn't outlive process startup on tmpfs.
	envFilePath = "/run/kvarn-runner.env"
)

type Cmd struct {
	Addr string `help:"Address to listen on for the local runner." default:":9090"`
}

func (c *Cmd) Run() error {
	return run(c.Addr)
}

// Connect is the in-VM entrypoint: it reads the bridge token + transport
// from the systemd-loaded EnvironmentFile, unlinks the file so the token
// doesn't persist on tmpfs, scrubs the token from its own environment, and
// dials the orchestrator's bridge service.
func Connect() error {
	token := os.Getenv(envBridgeToken)
	if token == "" {
		return fmt.Errorf("%s must be set", envBridgeToken)
	}
	// Drop the env var so anything we inherit from later (or anything that
	// reads /proc/<pid>/environ) doesn't see the bearer secret. The token is
	// already captured in a local; systemd has nothing more to read either.
	if err := os.Unsetenv(envBridgeToken); err != nil {
		slog.Warn("failed to unset bridge token env var", "error", err)
	}
	if err := os.Remove(envFilePath); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove env file after load", "path", envFilePath, "error", err)
	}

	var httpClient *http.Client
	var addr string

	if portStr := os.Getenv(envBridgeVsockPort); portStr != "" {
		port, err := strconv.ParseUint(portStr, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", envBridgeVsockPort, portStr, err)
		}
		httpClient, addr, err = vsockClient(uint32(port))
		if err != nil {
			return fmt.Errorf("vsock client: %w", err)
		}
	} else if a := os.Getenv(envBridgeAddr); a != "" {
		addr = a
	} else {
		return errors.New("either KVARN_BRIDGE_VSOCK_PORT or KVARN_BRIDGE_ADDR must be set")
	}

	if err := connectToOrchestrator(context.Background(), httpClient, addr, token); err != nil {
		reportFatalToConsole(err)
		return err
	}
	return nil
}

// consoleDevice is the guest's kernel console, which the host tails while the
// VM runs.
const consoleDevice = "/dev/console"

// reportFatalToConsole writes the reason the runner is giving up to the serial
// console. The runner's own log goes to the journal, which stays inside the
// VM; the console is what the host quotes when it reports the runner gone, so
// this is the one line that turns "runner disconnected" into a cause. The
// runner cannot come back after this: the token it registered with was
// unlinked at startup, so the restarted service has nothing to connect with.
func reportFatalToConsole(err error) {
	f, openErr := os.OpenFile(consoleDevice, os.O_WRONLY|syscall.O_NOCTTY, 0)
	if openErr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "kvarn-runner: fatal: %v\n", err)
}
