package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

type Cmd struct {
	Addr             string `help:"Address to listen on." default:":9090"`
	OrchestratorAddr string `help:"Orchestrator address to connect to (connect mode)." name:"orchestrator-addr"`
	Token            string `help:"Bootstrap token for orchestrator registration." name:"token"`
	VsockPort        uint32 `help:"Vsock port to connect to orchestrator." name:"vsock-port"`
}

func (c *Cmd) Run() error {
	if c.OrchestratorAddr != "" || c.VsockPort > 0 {
		if c.Token == "" {
			return errors.New("--token is required in connect mode")
		}

		var httpClient *http.Client
		var addr string

		if c.VsockPort > 0 {
			var err error
			httpClient, addr, err = vsockClient(c.VsockPort)
			if err != nil {
				return fmt.Errorf("vsock client: %w", err)
			}
		} else {
			addr = c.OrchestratorAddr
		}

		return connectToOrchestrator(context.Background(), httpClient, addr, c.Token)
	}

	return run(c.Addr)
}
