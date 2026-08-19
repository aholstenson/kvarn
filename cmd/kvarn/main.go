package main

import (
	"github.com/aholstenson/kvarn/internal/buildinfo"
	cachecmd "github.com/aholstenson/kvarn/internal/cmd/cache"
	imagecmd "github.com/aholstenson/kvarn/internal/cmd/image"
	imagecachecmd "github.com/aholstenson/kvarn/internal/cmd/imagecache"
	jobscmd "github.com/aholstenson/kvarn/internal/cmd/jobs"
	"github.com/aholstenson/kvarn/internal/cmd/key"
	localcmd "github.com/aholstenson/kvarn/internal/cmd/local"
	modescmd "github.com/aholstenson/kvarn/internal/cmd/modes"
	previewcmd "github.com/aholstenson/kvarn/internal/cmd/preview"
	queuecmd "github.com/aholstenson/kvarn/internal/cmd/queue"
	repocmd "github.com/aholstenson/kvarn/internal/cmd/repo"
	"github.com/aholstenson/kvarn/internal/cmd/secrets"
	versioncmd "github.com/aholstenson/kvarn/internal/cmd/version"
	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/orchestrator"
	"github.com/alecthomas/kong"
)

type CLI struct {
	Orchestrator orchestrator.Cmd  `cmd:"" help:"Run the orchestrator."`
	Jobs         jobscmd.Cmd       `cmd:"" help:"Start, list, inspect and manage jobs."`
	Queue        queuecmd.Cmd      `cmd:"" help:"Inspect the job queue."`
	Preview      previewcmd.Cmd    `cmd:"" help:"Start, list and inspect preview environments."`
	Modes        modescmd.Cmd      `cmd:"" help:"List the agent modes a job can run in."`
	Secrets      secrets.Cmd       `cmd:"" help:"Manage per-project secrets."`
	Key          key.Cmd           `cmd:"" help:"Manage API keys."`
	Local        localcmd.Cmd      `cmd:"" help:"Run jobs, tests and previews against the working directory in a local VM."`
	Image        imagecmd.Cmd      `cmd:"" help:"Manage the VM disk image."`
	Cache        cachecmd.Cmd      `cmd:"" help:"Inspect and clear tool caches."`
	ImageCache   imagecachecmd.Cmd `cmd:"" name:"image-cache" help:"Inspect and manage the pull-through OCI image cache."`
	Repo         repocmd.Cmd       `cmd:"" help:"Inspect and manage the host-side repository mirrors."`
	Version      versioncmd.Cmd    `cmd:"" help:"Print the kvarn version and build details."`

	// The flag form is what `--version` habitually reaches for; it prints the
	// bare version, while the subcommand prints the full build identity.
	VersionFlag kong.VersionFlag `name:"version" short:"V" help:"Print the version and exit."`
}

func main() {
	logging.Setup()

	var cli CLI
	ctx := kong.Parse(&cli, kong.UsageOnError(), kong.ConfigureHelp(kong.HelpOptions{
		Compact:             true,
		Summary:             true,
		NoExpandSubcommands: true,
	}), kong.Vars{"version": buildinfo.Version})
	err := ctx.Run()
	ctx.FatalIfErrorf(err)
}
