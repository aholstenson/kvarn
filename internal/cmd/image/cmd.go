package image

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/aholstenson/kvarn/internal/cmd/imageutil"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/cockroachdb/errors"
)

// Cmd is the parent command for `kvarn image <subcommand>`.
type Cmd struct {
	Pull PullCmd `cmd:"" help:"Download the VM disk image into the local cache."`
	Path PathCmd `cmd:"" help:"Print the resolved (or to-be-downloaded) image path."`
}

// PullCmd downloads the disk image into the user cache, overwriting any cached
// copy, so it can be pre-seeded for offline use.
type PullCmd struct {
	Version string `help:"Image version to download (e.g. v0.1.0). Defaults to the CLI build version." env:"KVARN_IMAGE_VERSION"`
	Arch    string `help:"Image architecture (arm64 or amd64). Defaults to the host architecture."`
}

func (c *PullCmd) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	path, err := vm.EnsureDiskImage(ctx, vm.DownloadOpts{
		Version:       c.Version,
		Arch:          c.Arch,
		ForceDownload: true,
		Progress:      imageutil.NewProgress(os.Stderr, "Downloading VM image…"),
	})
	if err != nil {
		return errors.Wrap(err, "pull image")
	}

	fmt.Fprintln(os.Stdout, path)
	return nil
}

// PathCmd prints the resolved disk image path, downloading it first unless
// --no-download is set.
type PathCmd struct {
	Version    string `help:"Image version to resolve (e.g. v0.1.0). Defaults to the CLI build version." env:"KVARN_IMAGE_VERSION"`
	Arch       string `help:"Image architecture (arm64 or amd64). Defaults to the host architecture."`
	NoDownload bool   `help:"Only resolve a local or cached image; never download." name:"no-download"`
}

func (c *PathCmd) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	path, err := vm.EnsureDiskImage(ctx, vm.DownloadOpts{
		Version:    c.Version,
		Arch:       c.Arch,
		NoDownload: c.NoDownload,
		Progress:   imageutil.NewProgress(os.Stderr, "Downloading VM image…"),
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stdout, path)
	return nil
}
