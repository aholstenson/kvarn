//go:build embedrunner

package runnerbin

import (
	_ "embed"
	"fmt"
	"runtime"
)

// runner is the linux runner binary baked into the CLI at build time. The
// orchestrator injects it into each VM at boot so the booted runner is always
// the exact one the orchestrator speaks to.
//
//go:embed kvarn-runner
var runner []byte

// Bytes returns the embedded runner binary for goarch. A CLI built for GOARCH
// X embeds only linux/X, since local providers always boot host-arch VMs, so a
// request for any other arch is a programming error.
func Bytes(goarch string) ([]byte, error) {
	if goarch != runtime.GOARCH {
		return nil, fmt.Errorf("embedded runner is for %s, not %s", runtime.GOARCH, goarch)
	}
	return runner, nil
}
