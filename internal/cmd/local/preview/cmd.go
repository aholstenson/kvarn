// Package preview implements `kvarn local preview`: bringing the preview
// environment declared in kvarn.yml up against the working tree, in a VM on
// this machine, reachable on loopback.
//
// It is the same preview the orchestrator serves — same sandbox, same serve
// steps, same ready checks — with the two things a developer's machine does not
// have taken out. There is no clone, because the working tree is the source,
// and there are no hostnames, because there is no domain: each app is forwarded
// to a loopback port instead. Everything the repository can get wrong about its
// preview is therefore wrong here too, which is the point of running it.
package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aholstenson/kvarn/internal/cmd/imageutil"
	"github.com/aholstenson/kvarn/internal/cmd/local/bootui"
	projectstore "github.com/aholstenson/kvarn/internal/config/project"
	projecttoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	"github.com/aholstenson/kvarn/internal/config/secret"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/preview"
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
	Verbose       bool   `help:"Show all output, including from passing steps." short:"v"`
	Logs          bool   `help:"Show log output." name:"logs"`
	Project       string `help:"Project name for secret lookup. Falls back to git remote → project store if omitted." short:"p"`
	SecretsFile   string `help:"Override path to secrets store (default: ~/.config/kvarn/secrets.toml)." name:"secrets-file"`

	Port map[string]uint16 `help:"Bind an app on a specific host port, as app=port. Repeatable." name:"port"`
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
		return errors.New("kvarn.yml declares no preview: add a `preview:` block with at least one app")
	}

	// Bind every app's host port before booting anything. A port collision is a
	// configuration problem, and discovering it after a VM has come up wastes a
	// boot; binding also fixes the URLs, which the serve commands need in their
	// environment before they start.
	apps, err := c.bindApps(cfg)
	if err != nil {
		return err
	}
	defer apps.closeListeners()

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

	// The app URLs go into the environment the whole VM sees, so setup steps and
	// ready checks read the same values the serve steps do.
	if secretEnv == nil {
		secretEnv = map[string]string{}
	}
	for _, app := range apps.apps {
		secretEnv[project.EnvVarName(app.Name)] = app.URL
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

	var cacheProvider cache.Provider
	var projectID string
	if !c.NoCache {
		fc, err := cache.DefaultFileCache()
		if err != nil {
			return fmt.Errorf("set up cache: %w", err)
		}
		cacheProvider = fc
		absDir, err := filepath.Abs(c.Dir)
		if err != nil {
			return fmt.Errorf("resolve absolute dir: %w", err)
		}
		projectID = cache.ProjectID(absDir)
	}

	boot := bootui.New(renderer)

	createOpts := vm.CreateOpts{Image: img}
	if len(managed) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(managedSecrets(managed), slog.Default())
	}

	sess, err := sandbox.Start(ctx, sandbox.Opts{
		Provider:      provider,
		CreateOpts:    createOpts,
		Config:        cfg,
		Transferer:    &transfer.StreamingTransferer{},
		SourceDir:     c.Dir,
		SkipFile:      skipFile,
		CacheProvider: cacheProvider,
		ProjectID:     projectID,
		Secrets:       secretEnv,
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

	serveItem := renderer.AddItem("Starting services")
	renderer.SetStatus(serveItem, taskui.StatusRunning, "")
	serveChildren := make(map[string]*taskui.Item, len(cfg.Preview.Serve))
	err = preview.StartServices(ctx, sess.Processes(), cfg, preview.ServeOpts{
		WorkspaceDir: sess.GetWorkingDir(),
		URLs:         apps.urls(),
		IDPrefix:     "local",
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
	forwards := apps.startForwards(sess.DialGuest)
	defer forwards.close()

	stopRenderer()
	apps.report(os.Stdout)
	logs.streamTo(os.Stdout)

	<-ctx.Done()
	fmt.Fprintln(os.Stdout, "\nStopping preview…")
	return nil
}

// boundApp is one app of the preview with its loopback listener already bound.
type boundApp struct {
	Name      string
	GuestPort uint16
	Listener  net.Listener
	URL       string
}

// boundApps is the preview's apps in stable name order.
type boundApps struct {
	apps []boundApp
}

// bindApps claims a loopback port for every declared app.
func (c *Cmd) bindApps(cfg *project.Config) (*boundApps, error) {
	names := make([]string, 0, len(cfg.Preview.Apps))
	for name := range cfg.Preview.Apps {
		names = append(names, name)
	}
	sort.Strings(names)

	for name := range c.Port {
		if _, ok := cfg.Preview.Apps[name]; !ok {
			return nil, fmt.Errorf("--port names app %q, which kvarn.yml does not declare", name)
		}
	}

	bound := &boundApps{}
	for _, name := range names {
		want := cfg.Preview.Apps[name].Port
		explicit := false
		if p, ok := c.Port[name]; ok {
			want = p
			explicit = true
		}

		ln, err := bindHostPort(want)
		if err != nil {
			bound.closeListeners()
			return nil, fmt.Errorf("bind a port for app %q: %w", name, err)
		}
		port := uint16(ln.Addr().(*net.TCPAddr).Port)
		// An explicit --port is a request, not a preference: silently serving
		// somewhere else would break whatever the caller pinned the port for.
		if explicit && port != want {
			ln.Close()
			bound.closeListeners()
			return nil, fmt.Errorf("port %d for app %q is already in use", want, name)
		}

		bound.apps = append(bound.apps, boundApp{
			Name:      name,
			GuestPort: cfg.Preview.Apps[name].Port,
			Listener:  ln,
			URL:       fmt.Sprintf("http://localhost:%d", port),
		})
	}
	return bound, nil
}

// urls maps app name to the address it answers on, for the serve environment.
func (b *boundApps) urls() map[string]string {
	out := make(map[string]string, len(b.apps))
	for _, app := range b.apps {
		out[app.Name] = app.URL
	}
	return out
}

// closeListeners releases the bound ports. It is the cleanup path for a preview
// that never started forwarding; once forwarders own the listeners, closing
// them is their job.
func (b *boundApps) closeListeners() {
	for _, app := range b.apps {
		if app.Listener != nil {
			app.Listener.Close()
		}
	}
	b.apps = nil
}

// startForwards hands every bound listener to a forwarder into the guest.
func (b *boundApps) startForwards(dial func(context.Context, uint16) (net.Conn, error)) *forwards {
	log := slog.With("component", "local-preview")
	f := &forwards{}
	for _, app := range b.apps {
		f.list = append(f.list, startForward(app.Listener, app.GuestPort, dial, log))
	}
	// The forwarders own the listeners now, so the deferred bind cleanup must
	// not close them a second time.
	b.apps = nil
	return f
}

// report prints the addresses the preview is serving on.
func (b *boundApps) report(w io.Writer) {
	fmt.Fprintln(w)
	width := 0
	for _, app := range b.apps {
		if len(app.Name) > width {
			width = len(app.Name)
		}
	}
	for _, app := range b.apps {
		fmt.Fprintf(w, "  %-*s  %s\n", width, app.Name, app.URL)
	}
	fmt.Fprintln(w, "\nPress Ctrl-C to stop.")
}

// forwards is every running port forward, closed together.
type forwards struct {
	list []*forwarder
}

func (f *forwards) close() {
	for _, fw := range f.list {
		fw.Close()
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
