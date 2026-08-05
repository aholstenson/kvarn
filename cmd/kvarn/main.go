package main

import (
	cachecmd "github.com/aholstenson/kvarn/internal/cmd/cache"
	imagecmd "github.com/aholstenson/kvarn/internal/cmd/image"
	imagecachecmd "github.com/aholstenson/kvarn/internal/cmd/imagecache"
	jobscmd "github.com/aholstenson/kvarn/internal/cmd/jobs"
	"github.com/aholstenson/kvarn/internal/cmd/key"
	modescmd "github.com/aholstenson/kvarn/internal/cmd/modes"
	queuecmd "github.com/aholstenson/kvarn/internal/cmd/queue"
	repocmd "github.com/aholstenson/kvarn/internal/cmd/repo"
	runcmd "github.com/aholstenson/kvarn/internal/cmd/run"
	"github.com/aholstenson/kvarn/internal/cmd/secrets"
	testcmd "github.com/aholstenson/kvarn/internal/cmd/test"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/alecthomas/kong"
)

type CLI struct {
	Orchestrator orchestrator.Cmd  `cmd:"" help:"Run the orchestrator."`
	Jobs         jobscmd.Cmd       `cmd:"" help:"Start, list, inspect and manage jobs."`
	Queue        queuecmd.Cmd      `cmd:"" help:"Inspect the job queue."`
	Modes        modescmd.Cmd      `cmd:"" help:"List the agent modes a job can run in."`
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
	ctx := kong.Parse(&cli, kong.UsageOnError(), kong.ConfigureHelp(kong.HelpOptions{
		Compact:             true,
		Summary:             true,
		NoExpandSubcommands: true,
	}))
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}
