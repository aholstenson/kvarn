package main

import (
	cachecmd "github.com/aholstenson/kvarn/internal/cmd/cache"
	cancelcmd "github.com/aholstenson/kvarn/internal/cmd/cancel"
	"github.com/aholstenson/kvarn/internal/cmd/feedback"
	imagecmd "github.com/aholstenson/kvarn/internal/cmd/image"
	imagecachecmd "github.com/aholstenson/kvarn/internal/cmd/imagecache"
	"github.com/aholstenson/kvarn/internal/cmd/key"
	repocmd "github.com/aholstenson/kvarn/internal/cmd/repo"
	runcmd "github.com/aholstenson/kvarn/internal/cmd/run"
	"github.com/aholstenson/kvarn/internal/cmd/secrets"
	"github.com/aholstenson/kvarn/internal/cmd/startjob"
	testcmd "github.com/aholstenson/kvarn/internal/cmd/test"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/alecthomas/kong"
)

type CLI struct {
	Orchestrator orchestrator.Cmd  `cmd:"" help:"Run the orchestrator."`
	StartJob     startjob.Cmd      `cmd:"" name:"startjob" help:"Start a project-aware job."`
	Feedback     feedback.Cmd      `cmd:"" help:"Continue work on an existing pull request."`
	Cancel       cancelcmd.Cmd     `cmd:"" help:"Cancel a running job."`
	Secrets      secrets.Cmd       `cmd:"" help:"Manage per-project secrets."`
	Key          key.Cmd           `cmd:"" help:"Manage API keys."`
	Run          runcmd.Cmd        `cmd:"" help:"Run the coding agent against the local working directory."`
	Test         testcmd.Cmd       `cmd:"" help:"Test project configuration in a local VM."`
	Image        imagecmd.Cmd      `cmd:"" help:"Manage the VM disk image."`
	Cache        cachecmd.Cmd      `cmd:"" help:"Inspect and clear tool caches."`
	ImageCache   imagecachecmd.Cmd `cmd:"" name:"image-cache" help:"Inspect and manage the pull-through OCI image cache."`
	Repo         repocmd.Cmd       `cmd:"" help:"Inspect and manage the host-side repository mirrors."`
}

func main() {
	logging.Setup()

	var cli CLI
	ctx := kong.Parse(&cli, kong.UsageOnError())
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}
