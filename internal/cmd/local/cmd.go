// Package local groups the commands that run against the working directory in
// a VM on this machine, with no orchestrator involved.
//
// The distinction the group draws is where the work happens, not what the work
// is. `kvarn jobs` and `kvarn preview` ask a running orchestrator to do
// something to a branch of a registered project; the commands here do the
// equivalent thing to the tree you are sitting in, on your own hardware, so a
// kvarn.yml can be got right before anything is pushed.
package local

import (
	jobcmd "github.com/aholstenson/kvarn/internal/cmd/local/job"
	previewcmd "github.com/aholstenson/kvarn/internal/cmd/local/preview"
	testcmd "github.com/aholstenson/kvarn/internal/cmd/local/test"
)

// Cmd is the parent command for `kvarn local <subcommand>`.
type Cmd struct {
	Test    testcmd.Cmd    `cmd:"" help:"Run the project's setup and validation steps in a local VM."`
	Job     jobcmd.Cmd     `cmd:"" help:"Run the coding agent against the local working directory."`
	Preview previewcmd.Cmd `cmd:"" help:"Serve the project's preview environment from a local VM."`
}
