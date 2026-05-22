//go:build !embedrunner

package runnerbin

import "github.com/cockroachdb/errors"

// Bytes returns the embedded linux runner binary for goarch.
//
// This is the default build: the runner is not compiled in, so callers get a
// clear error. Build the CLI with `task build` (which sets -tags embedrunner
// and stages internal/runnerbin/kvarn-runner) to embed it. The stub keeps
// `go build ./...` and `go test ./...` working without the generated artifact.
func Bytes(goarch string) ([]byte, error) {
	return nil, errors.New("runner not embedded in this build; build the CLI with `task build`")
}
