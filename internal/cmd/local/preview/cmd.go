// Package preview implements `kvarn local preview`: bringing the preview
// environment declared in kvarn.yml up against the working tree, in a VM on
// this machine, reachable on loopback.
//
// It is the same preview the orchestrator serves — same sandbox, same serve
// steps, same ready checks — with the one thing a developer's machine does not
// have taken out: there is no clone, because the working tree is the source.
// Everything the repository can get wrong about its preview is therefore wrong
// here too, which is the point of running it.
//
// How the sites are addressed is the developer's choice. By default each site
// is forwarded to a loopback port, and a server that virtual-hosts several
// sites tells them apart by that port rather than by name. Given
// --base-domain, the sites get the hostnames they would have when hosted, and
// one Host-routed listener serves all of them — which is the only way to
// exercise a repository whose behaviour depends on its own domain. Either way
// a site reads its address from KVARN_PREVIEW_URL_<SITE>.
package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/cmd/imageutil"
	"github.com/aholstenson/kvarn/internal/cmd/local/bootui"
	projectstore "github.com/aholstenson/kvarn/internal/config/project"
	projecttoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	"github.com/aholstenson/kvarn/internal/config/secret"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/nixpkgs"
	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/taskui"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/local"
)

// Cmd is the CLI command for running a project's preview environment locally.
type Cmd struct {
	DiskImagePath string `help:"Path to VM disk image. Auto-detected if not set."`
	Dir           string `help:"Project directory." default:"." type:"existingdir"`
	NoCache       bool   `help:"Disable cache persistence across runs." name:"no-cache"`
	Fresh         bool   `help:"Discard the preview's saved state before booting, so it comes up empty." name:"fresh"`
	NoState       bool   `help:"Do not save the preview's state on the way out." name:"no-state"`
	Verbose       bool   `help:"Show all output, including from passing steps." short:"v"`
	Logs          bool   `help:"Show log output." name:"logs"`
	Project       string `help:"Project name for secret lookup. Falls back to git remote → project store if omitted." short:"p"`
	SecretsFile   string `help:"Override path to secrets store (default: ~/.config/kvarn/secrets.toml)." name:"secrets-file"`

	Port map[string]uint16 `help:"Bind a site on a specific host port, as site=port. Repeatable." name:"port"`

	BaseDomain  string `help:"Serve the sites on hostnames under this domain, e.g. sws.local, instead of loopback ports. The names have to resolve to 127.0.0.1 on this machine." name:"base-domain"`
	Ref         string `help:"Ref label the site hostnames are formed with, for the {ref} part of a site's host pattern." default:"local" name:"ref"`
	PR          string `help:"Pull request the site hostnames are formed with, for the {pr} part of a site's host pattern." default:"local" name:"pr"`
	IngressPort uint16 `help:"Host port the sites are served on with --base-domain. Defaults to the port the sites share inside the VM, else 8080." name:"ingress-port"`
}

// DefaultRefLabel is the {ref} — and, for a repository whose sites are named by
// pull request, the {pr} — a local preview's hostnames are formed with. It is a
// fixed word rather than the checked-out branch so the names — and therefore
// /etc/hosts entries and anything registered against them, such as OAuth
// redirect URLs — survive switching branches.
const DefaultRefLabel = "local"

// refLabel is the ref this preview is keyed on: the same one its hostnames are
// formed with, so switching branches does not switch which saved state comes
// back.
func (c *Cmd) refLabel() string {
	if c.Ref == "" {
		return DefaultRefLabel
	}
	return c.Ref
}

func (c *Cmd) Run() error {
	// Redirect slog to discard unless --logs is passed.
	if !c.Logs {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Ctrl-C is the normal way out of this command: it runs until told to stop.
	// The cancel it triggers unwinds the deferred teardown below rather than
	// killing the process, so the VM is actually stopped.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Resolve (and if needed download) the disk image before the TUI starts so
	// any download progress goes to stderr without corrupting the renderer.
	diskImagePath, err := vm.EnsureDiskImage(ctx, vm.DownloadOpts{
		Path:     c.DiskImagePath,
		Progress: imageutil.NewProgress(os.Stderr, "Downloading VM image…"),
	})
	if err != nil {
		return fmt.Errorf("find disk image: %w", err)
	}

	cfg, err := project.Load(c.Dir)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		return errors.New("no kvarn.yml found in project directory")
	}
	if !cfg.Preview.Enabled() {
		return errors.New("kvarn.yml declares no preview: add a `preview:` block with at least one site")
	}

	// Settle how the sites are reached before booting anything. A port
	// collision is a configuration problem, and discovering it after a VM has
	// come up wastes a boot; binding also fixes the URLs, which the serve
	// commands need in their environment before they start.
	sites, err := c.planSites(ctx, cfg)
	if err != nil {
		return err
	}
	defer sites.closeListeners()

	// Resolve any secrets declared in kvarn.yml against the per-project secret
	// store, exactly as a job or an orchestrator-hosted preview would. Note what
	// that means: the servers this starts are ordinary project code with the
	// project's real credentials behind the egress proxy.
	var secretEnv map[string]string
	var managed map[string]secret.Managed
	if len(cfg.Secrets) > 0 {
		projectName, err := c.resolveProjectName(ctx)
		if err != nil {
			return err
		}
		store, err := openSecretStore(c.SecretsFile)
		if err != nil {
			return err
		}
		secretEnv, managed, err = secret.Resolve(ctx, store, projectName, secretRefs(cfg.Secrets))
		if err != nil {
			return fmt.Errorf("resolve secrets: %w", err)
		}
	}

	// The preview's environment goes into the environment the whole VM sees, so
	// setup steps and ready checks read the same values the serve steps do.
	if secretEnv == nil {
		secretEnv = map[string]string{}
	}
	previewEnv := preview.Env(sites.urls())
	for name, value := range previewEnv {
		secretEnv[name] = value
	}

	renderer := taskui.New(os.Stdout, c.Verbose)
	renderer.Start()
	rendererStopped := false
	stopRenderer := func() {
		if !rendererStopped {
			rendererStopped = true
			renderer.Stop()
		}
	}
	defer stopRenderer()

	provider := local.NewProvider()
	img, err := provider.PrepareImage(ctx, vm.BaseImage{DiskImagePath: diskImagePath})
	if err != nil {
		return fmt.Errorf("prepare image: %w", err)
	}

	skipFile, err := transfer.GitIgnoreFilter(c.Dir)
	if err != nil {
		return fmt.Errorf("set up gitignore filter: %w", err)
	}

	absDir, err := filepath.Abs(c.Dir)
	if err != nil {
		return fmt.Errorf("resolve absolute dir: %w", err)
	}

	var cacheProvider cache.Provider
	var projectID string
	if !c.NoCache {
		fc, err := cache.DefaultFileCache()
		if err != nil {
			return fmt.Errorf("set up cache: %w", err)
		}
		cacheProvider = fc
		projectID = cache.ProjectID(absDir)
	}

	// State is not the tool cache and --no-cache does not govern it: one is a
	// rebuild's worth of work, the other is whatever somebody entered into the
	// preview. --fresh is what ignores this one.
	states, err := snapshot.DefaultDir()
	if err != nil {
		return fmt.Errorf("find preview state directory: %w", err)
	}
	stateStore := snapshot.NewFileStore(states)
	stateID := snapshot.ID{
		ProjectID: cache.ProjectID(absDir),
		RefLabel:  project.RefLabel(c.refLabel()),
	}
	if c.Fresh {
		if err := stateStore.Delete(stateID); err != nil {
			return fmt.Errorf("discard saved preview state: %w", err)
		}
	}

	boot := bootui.New(renderer)

	createOpts := vm.CreateOpts{Image: img}
	if len(managed) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(managedSecrets(managed), slog.Default())
	}

	sess, err := sandbox.Start(ctx, sandbox.Opts{
		Provider:      provider,
		CreateOpts:    createOpts,
		NixpkgsRev:    nixpkgs.Shared().Rev,
		Config:        cfg,
		Transferer:    &transfer.StreamingTransferer{},
		SourceDir:     c.Dir,
		SkipFile:      skipFile,
		CacheProvider: cacheProvider,
		ProjectID:     projectID,
		Secrets:       secretEnv,
		HostAliases:   sites.guestAliases(),
		OnEvent:       boot.OnEvent,
	})
	if err != nil {
		return err
	}
	defer func() {
		item := renderer.AddItem("Shutting down sandbox")
		renderer.SetStatus(item, taskui.StatusRunning, "")
		sess.Close()
		renderer.SetStatus(item, taskui.StatusPassed, "")
	}()

	boot.Done()

	// Serving a preview means carrying browser traffic into the guest, which
	// only some providers can do. Finding that out here, rather than when the
	// first request hangs, is the difference between a clear message and a
	// mystery.
	if !sess.CanDialGuest() {
		return fmt.Errorf("the %s VM provider cannot carry preview traffic: %w",
			provider.Name(), errors.ErrUnsupported)
	}

	if len(cfg.Setup.Steps) > 0 || len(cfg.Setup.HealthChecks) > 0 {
		setupItem := renderer.AddItem("Setup")
		renderer.SetStatus(setupItem, taskui.StatusRunning, "")
		steps := append(append([]project.Step{}, cfg.Setup.Steps...), cfg.Setup.HealthChecks...)
		onDone, onOutput := bootui.StepCallbacks(renderer, setupItem, steps, nil)
		if _, err := sess.RunSetup(ctx, cfg, onDone, onOutput); err != nil {
			renderer.SetStatus(setupItem, taskui.StatusFailed, "")
			return fmt.Errorf("setup: %w", err)
		}
		renderer.SetStatus(setupItem, bootui.ParentStatus(setupItem), "")
	}

	// Output from the servers is kept rather than streamed: the task UI owns the
	// terminal while the preview comes up, and a server's startup chatter is
	// wanted only when something goes wrong. Once everything is ready the buffer
	// is drained to stdout and later output follows it live.
	logs := newServiceLog()

	// The preview's environment goes into the shell before any preview step
	// runs: a step that configures domains needs the names it is configuring, a
	// ready check that curls one needs the same, and a restore hook needs to know
	// where its dump was unpacked to.
	if err := preview.ExportEnv(ctx, sess.GetRunner(), sess.GetShellSessionID(), previewEnv); err != nil {
		stopRenderer()
		return err
	}

	// What the last run left behind goes back before the preview's own setup: a
	// stack that bind-mounts a database volume out of the state directory has to
	// find it populated by the time its containers come up.
	if err := c.restoreState(ctx, renderer, sess, cfg, stateStore, stateID, logs); err != nil {
		stopRenderer()
		logs.dump(os.Stdout)
		return err
	}

	if len(cfg.Preview.Setup) > 0 {
		previewSetupItem := renderer.AddItem("Preview setup")
		renderer.SetStatus(previewSetupItem, taskui.StatusRunning, "")
		children := make(map[string]*taskui.Item, len(cfg.Preview.Setup))
		var running *taskui.Item
		err := preview.RunSetup(ctx, sess.GetRunner(), sess.GetShellSessionID(), cfg.Preview.Setup,
			func(name string) {
				child := renderer.AddChild(previewSetupItem, name)
				renderer.SetStatus(child, taskui.StatusRunning, "")
				children[name] = child
				running = child
			},
			func(name, stdout, stderr string) {
				logs.write(name, stdout)
				logs.write(name, stderr)
			},
			func(name string) {
				renderer.SetStatus(children[name], taskui.StatusPassed, "")
				running = nil
			},
		)
		if err != nil {
			if running != nil {
				renderer.SetStatus(running, taskui.StatusFailed, "")
			}
			renderer.SetStatus(previewSetupItem, taskui.StatusFailed, "")
			stopRenderer()
			logs.dump(os.Stdout)
			return err
		}
		renderer.SetStatus(previewSetupItem, taskui.StatusPassed, "")
	}

	// A repository whose servers are already up by the end of setup — a
	// container stack, say — declares no serve steps, and there is nothing to
	// report starting.
	if len(cfg.Preview.Serve) > 0 {
		serveItem := renderer.AddItem("Starting services")
		renderer.SetStatus(serveItem, taskui.StatusRunning, "")
		serveChildren := make(map[string]*taskui.Item, len(cfg.Preview.Serve))
		err = preview.StartServices(ctx, sess.Processes(), cfg, preview.ServeOpts{
			WorkspaceDir: sess.GetWorkingDir(),
			Env:          previewEnv,
			IDPrefix:     localProcessPrefix,
			OnStarting: func(name string) {
				item := renderer.AddChild(serveItem, name)
				renderer.SetStatus(item, taskui.StatusRunning, "")
				serveChildren[name] = item
			},
			OnOutput: func(name, stdout, stderr string) {
				logs.write(name, stdout)
				logs.write(name, stderr)
			},
			OnExit: func(name string, exitCode int32, exitErr error) {
				// A server that dies is reported where the reader is looking. Before
				// the preview is up that is the task UI; after it, the log stream.
				if item, ok := serveChildren[name]; ok && !logs.streaming() {
					renderer.SetStatus(item, taskui.StatusFailed,
						fmt.Sprintf("exited with status %s", preview.FormatExitCode(exitCode)))
				}
				if exitErr != nil {
					logs.note(fmt.Sprintf("%s exited: %v", name, exitErr))
					return
				}
				logs.note(fmt.Sprintf("%s exited with status %s", name, preview.FormatExitCode(exitCode)))
			},
		})
		if err != nil {
			renderer.SetStatus(serveItem, taskui.StatusFailed, "")
			stopRenderer()
			logs.dump(os.Stdout)
			return err
		}
		renderer.SetStatus(serveItem, taskui.StatusPassed, "")
	}

	if len(cfg.Preview.Ready) > 0 {
		readyItem := renderer.AddItem("Ready checks")
		renderer.SetStatus(readyItem, taskui.StatusRunning, "")
		children := make(map[string]*taskui.Item, len(cfg.Preview.Ready))
		for _, step := range cfg.Preview.Ready {
			child := renderer.AddChild(readyItem, step.Name)
			child.Status = taskui.StatusPending
			children[step.Name] = child
		}
		// The first check is the one that waits for the server to bind, so it is
		// marked running immediately; each later one starts when the one before
		// it passes.
		if len(cfg.Preview.Ready) > 0 {
			renderer.SetStatus(children[cfg.Preview.Ready[0].Name], taskui.StatusRunning, "")
		}
		next := 1
		err := preview.WaitReady(ctx, sess.GetRunner(), sess.GetShellSessionID(), cfg.Preview.Ready, func(name string) {
			renderer.SetStatus(children[name], taskui.StatusPassed, "")
			if next < len(cfg.Preview.Ready) {
				renderer.SetStatus(children[cfg.Preview.Ready[next].Name], taskui.StatusRunning, "")
				next++
			}
		})
		if err != nil {
			renderer.SetStatus(readyItem, taskui.StatusFailed, "")
			stopRenderer()
			logs.dump(os.Stdout)
			return err
		}
		renderer.SetStatus(readyItem, taskui.StatusPassed, "")
	}

	// Only start forwarding once the preview says it is ready: a browser opened
	// on a URL that is not serving yet shows a connection error, and the URLs are
	// printed at the same moment they start working.
	serving := sites.serve(sess.DialGuest, func(name string, guestPort uint16, err error) {
		// Everything else looks healthy in this case — the boot passed and the
		// host port is bound — so without a word here the only symptom is a
		// connection reset, which reads as kvarn never having bound anything.
		logs.note(fmt.Sprintf(
			"%s: nothing is listening on port %d inside the VM. "+
				"Check that the preview's server binds that port (%v).",
			name, guestPort, err))
	})
	defer serving.close()

	stopRenderer()
	sites.report(os.Stdout)
	logs.streamTo(os.Stdout)

	<-ctx.Done()
	fmt.Fprintln(os.Stdout, "\nStopping preview…")
	c.captureState(sess, cfg, stateStore, stateID)
	return nil
}

// localProcessPrefix namespaces the guest process IDs of a local preview's
// serve steps. Stopping them on the way out needs the same prefix that started
// them, so it is a constant rather than a literal in two places.
const localProcessPrefix = "local"

// restoreState puts back what the previous run of this preview left behind. It
// gets a task item of its own beside "Preview setup", because unpacking a
// database is slow enough that silence would read as a hang.
func (c *Cmd) restoreState(
	ctx context.Context,
	renderer *taskui.Renderer,
	sess *sandbox.Session,
	cfg *project.Config,
	store snapshot.Store,
	id snapshot.ID,
	logs *serviceLog,
) error {
	proxy := sess.BareRunner()
	if proxy == nil {
		return fmt.Errorf("this sandbox cannot carry preview state into the guest: %w", errors.ErrUnsupported)
	}
	if err := preview.PrepareStateDir(ctx, proxy); err != nil {
		return err
	}

	if _, err := store.Stat(id); errors.Is(err, snapshot.ErrNoSnapshot) {
		return nil
	}

	item := renderer.AddItem("Restoring saved state")
	renderer.SetStatus(item, taskui.StatusRunning, "")
	_, err := preview.Restore(ctx, preview.RestoreOpts{
		Proxy:          proxy,
		Runner:         sess.GetRunner(),
		ShellSessionID: sess.GetShellSessionID(),
		Store:          store,
		ID:             id,
		State:          cfg.Preview.State,
		OnOutput: func(name, stdout, stderr string) {
			logs.write(name, stdout)
			logs.write(name, stderr)
		},
	})
	if err != nil {
		renderer.SetStatus(item, taskui.StatusFailed, "")
		return fmt.Errorf("restore saved state (use --fresh to start empty): %w", err)
	}
	renderer.SetStatus(item, taskui.StatusPassed, "")
	return nil
}

// captureState writes the preview's declared state out before the VM goes away.
//
// It cannot use the command's own context: that comes from the interrupt
// handler and is already cancelled by the time this runs. It also cannot use
// the task UI, which stopped when the preview came up — so it reports itself in
// plain lines beside "Stopping preview…".
func (c *Cmd) captureState(sess *sandbox.Session, cfg *project.Config, store snapshot.Store, id snapshot.ID) {
	if c.NoState {
		return
	}
	proxy := sess.BareRunner()
	if proxy == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), localStateTimeout)
	defer cancel()

	st := cfg.Preview.State
	if !st.Declared() {
		has, err := preview.HasState(ctx, proxy, st)
		if err != nil || !has {
			return
		}
	}

	fmt.Fprintln(os.Stdout, "Saving preview state…")
	if err := preview.StopServices(ctx, sess.Processes(), cfg, localProcessPrefix, preview.DefaultStopGrace); err != nil {
		fmt.Fprintf(os.Stderr, "Could not stop the preview's services first: %v\n", err)
	}

	meta, err := preview.Capture(ctx, preview.CaptureOpts{
		Proxy:          proxy,
		Runner:         sess.GetRunner(),
		ShellSessionID: sess.GetShellSessionID(),
		Store:          store,
		ID:             id,
		State:          st,
		MaxBytes:       st.MaxSizeBytes(),
		Meta:           snapshot.Meta{Commit: sess.GetBaseCommit(), Ref: c.refLabel()},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not save the preview's state: %v\n", err)
		return
	}
	if meta.CreatedAt.IsZero() {
		return
	}
	fmt.Fprintf(os.Stdout, "Saved preview state (%s). The next run restores it; --fresh starts empty.\n",
		formatBytes(meta.Bytes))
}

// localStateTimeout bounds the capture on the way out of `kvarn local preview`.
// Ctrl-C should end in a preview that was saved, but a developer who presses it
// twice is entitled to have the command actually stop.
const localStateTimeout = 2 * time.Minute

// formatBytes renders an archive size in the units a person reads.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// resolveProjectName figures out which project name to use when looking up
// secrets: the --project flag, else a best-effort match of the checkout's origin
// remote against the project store.
func (c *Cmd) resolveProjectName(ctx context.Context) (string, error) {
	if c.Project != "" {
		return c.Project, nil
	}

	if remote, ok := gitOriginURL(c.Dir); ok {
		store := projecttoml.New(projecttoml.DefaultPath())
		if projects, err := store.List(ctx); err == nil {
			var matches []*projectstore.Project
			for _, p := range projects {
				if p.RepoURL == remote {
					matches = append(matches, p)
				}
			}
			if len(matches) == 1 {
				return matches[0].Name, nil
			}
		}
	}

	return "", errors.New(
		"kvarn.yml declares secrets but no project is configured for this checkout. " +
			"Pass --project <name>, or register the project and add secrets with " +
			"`kvarn projects add` and `kvarn secrets set <project> <NAME>`.")
}

// gitOriginURL returns remote.origin.url for the repository containing dir, or
// ("", false) if git is unavailable, dir is not a repository, or no origin
// remote is set.
func gitOriginURL(dir string) (string, bool) {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(out))
	return url, url != ""
}

// secretRefs maps kvarn.yml secret declarations to resolution refs.
func secretRefs(refs []project.SecretRef) []secret.Ref {
	out := make([]secret.Ref, len(refs))
	for i, r := range refs {
		out[i] = secret.Ref{Name: r.Name, Scheme: r.Scheme, Hosts: r.Hosts}
	}
	return out
}

// managedSecrets translates resolved managed secrets into the proxy's injector
// input.
func managedSecrets(m map[string]secret.Managed) map[string]egressproxy.ManagedSecret {
	out := make(map[string]egressproxy.ManagedSecret, len(m))
	for ph, ms := range m {
		out[ph] = egressproxy.ManagedSecret{
			Value:  ms.Value,
			Scheme: egressproxy.Scheme(ms.Scheme),
			Hosts:  ms.Hosts,
		}
	}
	return out
}

// openSecretStore opens the per-project secret store, surfacing a friendlier
// error than a raw file-not-found when the store has never been initialised.
func openSecretStore(path string) (secret.Store, error) {
	store := secrettoml.OpenDefault(path)
	resolved := path
	if resolved == "" {
		resolved = secrettoml.DefaultPath()
	}
	if _, err := os.Stat(resolved); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"no secret store at %s. Declare secrets with "+
					"`kvarn secrets set <project> <NAME>` before running `kvarn local preview`.",
				resolved)
		}
		return nil, fmt.Errorf("stat %s: %w", resolved, err)
	}
	return store, nil
}
