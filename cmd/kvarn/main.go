package main

import (
	imagecmd "github.com/aholstenson/kvarn/internal/cmd/image"
	runcmd "github.com/aholstenson/kvarn/internal/cmd/run"
	"github.com/aholstenson/kvarn/internal/cmd/secrets"
	"github.com/aholstenson/kvarn/internal/cmd/startjob"
	testcmd "github.com/aholstenson/kvarn/internal/cmd/test"
	"github.com/aholstenson/kvarn/internal/cmd/verify"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/alecthomas/kong"
)

type CLI struct {
	Orchestrator orchestrator.Cmd `cmd:"" help:"Run the orchestrator."`
	StartJob     startjob.Cmd     `cmd:"" name:"startjob" help:"Start a project-aware job."`
	Secrets      secrets.Cmd      `cmd:"" help:"Manage per-project secrets."`
	Run          runcmd.Cmd       `cmd:"" help:"Run the coding agent against the local working directory."`
	Test         testcmd.Cmd      `cmd:"" help:"Test project configuration in a local VM."`
	Image        imagecmd.Cmd     `cmd:"" help:"Manage the VM disk image."`
	Verify       verify.Cmd       `cmd:"" help:"Verify the orchestrator and runner pipeline work correctly."`
}

func main() {
	logging.Setup()

	var cli CLI
	ctx := kong.Parse(&cli, kong.UsageOnError())
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}
