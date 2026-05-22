package test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"errors"
	"github.com/aholstenson/kvarn/internal/cmd/imageutil"
	projectstore "github.com/aholstenson/kvarn/internal/config/project"
	projecttoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	"github.com/aholstenson/kvarn/internal/config/secret"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/taskui"
	"github.com/aholstenson/kvarn/internal/vm"

	"github.com/aholstenson/kvarn/internal/vm/local"
)

// Cmd is the CLI command for testing project configuration locally.
type Cmd struct {
	DiskImagePath string `help:"Path to VM disk image. Auto-detected if not set."`
	Dir           string `help:"Project directory." default:"." type:"existingdir"`
	NoCache       bool   `help:"Disable cache persistence across runs." name:"no-cache"`
	Verbose       bool   `help:"Show all output, including from passing steps." short:"v"`
	Logs          bool   `help:"Show log output." name:"logs"`
	Project       string `help:"Project name for secret lookup. Falls back to git remote → project store if omitted." short:"p"`
	SecretsFile   string `help:"Override path to secrets store (default: ~/.config/kvarn/secrets.toml)." name:"secrets-file"`
}

func (c *Cmd) Run() error {
	// Redirect slog to discard unless --logs is passed.
	if !c.Logs {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

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

	renderer := taskui.New(os.Stdout, c.Verbose)
	renderer.Start()

	// Load project config.
	cfg, err := project.Load(c.Dir)
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		renderer.Stop()
		return errors.New("no kvarn.yml found in project directory")
	}

	// Resolve any secrets declared in kvarn.yml against the per-project
	// secret store. env-typed secrets become real env vars in the VM;
	// bearer-typed secrets are exposed as per-job placeholders that the
	// local egress proxy substitutes for the real value just before the
	// request leaves the host.
	var secretEnv, bearerPlaceholders map[string]string
	if len(cfg.Secrets) > 0 {
		projectName, err := c.resolveProjectName(ctx)
		if err != nil {
			renderer.Stop()
			return err
		}

		store, err := openSecretStore(c.SecretsFile)
		if err != nil {
			renderer.Stop()
			return err
		}

		secretEnv, bearerPlaceholders, err = secret.Resolve(ctx, store, projectName, cfg.Secrets)
		if err != nil {
			renderer.Stop()
			return fmt.Errorf("resolve secrets: %w", err)
		}
	}

	// Prepare VM image.
	provider := local.NewProvider()
	img, err := provider.PrepareImage(ctx, vm.BaseImage{DiskImagePath: diskImagePath})
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("prepare image: %w", err)
	}

	// Set up file transfer with gitignore filtering.
	skipFile, err := transfer.GitIgnoreFilter(c.Dir)
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("set up gitignore filter: %w", err)
	}

	// Resolve cache provider unless --no-cache is set.
	var cacheProvider cache.Provider
	var projectID string
	if !c.NoCache {
		fc, err := cache.DefaultFileCache()
		if err != nil {
			renderer.Stop()
			return fmt.Errorf("set up cache: %w", err)
		}
		cacheProvider = fc
		absDir, err := filepath.Abs(c.Dir)
		if err != nil {
			renderer.Stop()
			return fmt.Errorf("resolve absolute dir: %w", err)
		}
		projectID = cache.ProjectID(absDir)
	}

	// Track the last provisioning item for implicit completion.
	var lastProvisionItem *taskui.Item
	var dependenciesItem *taskui.Item

	markLastProvisionDone := func() {
		if lastProvisionItem != nil {
			renderer.SetStatus(lastProvisionItem, taskui.StatusPassed, "")
			lastProvisionItem = nil
		}
	}

	// Wire bearer placeholders into the local egress proxy so outbound
	// HTTP requests get the real secret substituted in.
	createOpts := vm.CreateOpts{Image: img}
	if len(bearerPlaceholders) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(bearerPlaceholders, slog.Default())
	}

	// Boot VM, transfer files, configure sandbox.
	sess, err := sandbox.Start(ctx, sandbox.Opts{
		Provider:      provider,
		CreateOpts:    createOpts,
		Config:        cfg,
		Transferer:    &transfer.StreamingTransferer{SkipFile: skipFile},
		SourceDir:     c.Dir,
		CacheProvider: cacheProvider,
		ProjectID:     projectID,
		Secrets:       secretEnv,
		OnEvent: func(e sandbox.Event) {
			switch ev := e.(type) {
			case sandbox.ProvisioningEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem("Provisioning VM")
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.TransferringEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem("Transferring files")
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.TransferProgressEvent:
				if lastProvisionItem != nil {
					lastProvisionItem.Suffix = fmt.Sprintf("%.1f MB / %.1f MB",
						float64(ev.BytesSent)/(1024*1024),
						float64(ev.TotalBytes)/(1024*1024))
				}
			case sandbox.DependenciesInstallingEvent:
				markLastProvisionDone()
				dependenciesItem = renderer.AddItem("Installing dependencies")
				renderer.SetStatus(dependenciesItem, taskui.StatusRunning, "")
				lastProvisionItem = dependenciesItem
			case sandbox.DependencyOutputEvent:
				if dependenciesItem != nil {
					for _, line := range strings.Split(strings.TrimRight(ev.Stdout, "\n"), "\n") {
						if line != "" {
							renderer.AppendOutput(dependenciesItem, line)
						}
					}
					for _, line := range strings.Split(strings.TrimRight(ev.Stderr, "\n"), "\n") {
						if line != "" {
							renderer.AppendOutput(dependenciesItem, line)
						}
					}
				}
			case sandbox.DependenciesInstalledEvent:
				if dependenciesItem != nil {
					renderer.SetStatus(dependenciesItem, taskui.StatusPassed, "")
					if lastProvisionItem == dependenciesItem {
						lastProvisionItem = nil
					}
					dependenciesItem = nil
				}
			case sandbox.ImagePullingEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem(fmt.Sprintf("Pulling image %s", ev.Image))
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.ContainerStartingEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem("Starting container")
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.ContainerStartedEvent:
				markLastProvisionDone()
			case sandbox.CacheRestoringEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem("Restoring cache")
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.CacheProgressEvent:
				if lastProvisionItem != nil {
					lastProvisionItem.Suffix = fmt.Sprintf("%s (%d/%d)", ev.Path, ev.Index, ev.Total)
				}
			case sandbox.CacheRestoredEvent:
				markLastProvisionDone()
			case sandbox.CacheSavingEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem("Saving cache")
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.CacheSavedEvent:
				markLastProvisionDone()
			case sandbox.SessionCreatingEvent:
				markLastProvisionDone()
				lastProvisionItem = renderer.AddItem("Creating shell session")
				renderer.SetStatus(lastProvisionItem, taskui.StatusRunning, "")
			case sandbox.SessionCreatedEvent:
				markLastProvisionDone()
			}
		},
	})
	if err != nil {
		renderer.Stop()
		return err
	}
	// shutdown tears the sandbox down as a visible step. It is idempotent so
	// the success path can render it before printing the summary, while the
	// defer still guarantees cleanup on early returns. The renderer must be
	// stopped before any raw stdout (the summary) is written, otherwise that
	// output races the spinner and corrupts the redraw.
	shutdownDone := false
	shutdown := func() {
		if shutdownDone {
			return
		}
		shutdownDone = true
		closeItem := renderer.AddItem("Shutting down sandbox")
		renderer.SetStatus(closeItem, taskui.StatusRunning, "")
		sess.Close()
		renderer.SetStatus(closeItem, taskui.StatusPassed, "")
	}
	defer func() {
		shutdown()
		renderer.Stop()
	}()

	markLastProvisionDone()

	/*
		// Print VM info.
		if vi := sess.VmInfo; vi != nil {
			var info strings.Builder
			info.WriteString("\n")
			if vi.CpuCount > 0 {
				info.WriteString(fmt.Sprintf("  CPU:    %d cores", vi.CpuCount))
				if vi.CpuModel != "" {
					info.WriteString(fmt.Sprintf(" (%s)", vi.CpuModel))
				}
				info.WriteString("\n")
			}
			if vi.MemTotalMb > 0 {
				info.WriteString(fmt.Sprintf("  Memory: %d MB available / %d MB total\n", vi.MemAvailableMb, vi.MemTotalMb))
			}
			if vi.DiskTotalMb > 0 {
				info.WriteString(fmt.Sprintf("  Disk:   %d MB used / %d MB total\n", vi.DiskUsedMb, vi.DiskTotalMb))
			}
			info.WriteString("\n")
			renderer.WriteRaw(info.String())
		}
	*/

	// Run steps and collect results.
	var summary summaryState

	// makeCallbacks creates step callbacks that add children under a parent item.
	// It pre-populates all children in pending state so upcoming steps are visible.
	makeCallbacks := func(parent *taskui.Item, steps []project.Step) (
		func(sandbox.StepResult, string),
		func(string, string, string, string),
	) {
		childItems := make(map[string]*taskui.Item, len(steps))
		for _, step := range steps {
			child := renderer.AddChild(parent, step.Name)
			child.Status = taskui.StatusPending
			childItems[step.Name] = child
		}

		outputCb := func(stepName string, _ string, stdout string, stderr string) {
			item, ok := childItems[stepName]
			if !ok {
				item = renderer.AddChild(parent, stepName)
				childItems[stepName] = item
			}
			if item.Status == taskui.StatusPending {
				renderer.SetStatus(item, taskui.StatusRunning, "")
			}
			for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
				if line != "" {
					renderer.AppendOutput(item, line)
				}
			}
			for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
				if line != "" {
					renderer.AppendOutput(item, line)
				}
			}
		}

		stepDone := func(result sandbox.StepResult, _ string) {
			item, ok := childItems[result.Name]
			if !ok {
				item = renderer.AddChild(parent, result.Name)
				childItems[result.Name] = item
			}

			if result.Skipped {
				summary.skipped++
				renderer.SetStatus(item, taskui.StatusSkipped, "(no matching files)")
			} else if result.ExitCode != 0 || result.Err != nil {
				summary.failed++
				renderer.SetStatus(item, taskui.StatusFailed, "")

				// Save details for reprinting after the TUI.
				hasOutput := len(item.Output) > 0
				if !hasOutput {
					summary.failedDetails = append(summary.failedDetails, stepOutput{
						name:   result.Name,
						stdout: result.Stdout,
						stderr: result.Stderr,
						err:    result.Err,
					})
				} else if result.Err != nil {
					summary.failedDetails = append(summary.failedDetails, stepOutput{
						name: result.Name,
						err:  result.Err,
					})
				}
			} else {
				summary.passed++
				renderer.SetStatus(item, taskui.StatusPassed, "")
			}

			delete(childItems, result.Name)
		}

		return stepDone, outputCb
	}

	// parentStatus determines the overall status from child items.
	parentStatus := func(parent *taskui.Item) taskui.Status {
		for _, child := range parent.Children {
			if child.Status == taskui.StatusFailed {
				return taskui.StatusFailed
			}
		}
		return taskui.StatusPassed
	}

	// Run setup.
	if len(cfg.Setup.Steps) > 0 {
		setupItem := renderer.AddItem("Setup")
		renderer.SetStatus(setupItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(setupItem, cfg.Setup.Steps)
		_, err := sess.RunSetup(ctx, cfg, stepDone, outputCb)
		if err != nil {
			renderer.Stop()
			return summary.finish(err)
		}
		renderer.SetStatus(setupItem, parentStatus(setupItem), "")
	}

	// Run health checks.
	if len(cfg.Setup.HealthChecks) > 0 {
		healthItem := renderer.AddItem("Health Checks")
		renderer.SetStatus(healthItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(healthItem, cfg.Setup.HealthChecks)
		_, err := sess.RunSetup(ctx, &project.Config{
			Setup: project.Setup{HealthChecks: cfg.Setup.HealthChecks},
		}, stepDone, outputCb)
		if err != nil {
			renderer.Stop()
			return summary.finish(err)
		}
		renderer.SetStatus(healthItem, parentStatus(healthItem), "")
	}

	// Run required validation.
	if len(cfg.Validation.Required) > 0 {
		reqItem := renderer.AddItem("Validation (required)")
		renderer.SetStatus(reqItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(reqItem, cfg.Validation.Required)
		valResult, err := sess.RunValidation(ctx, &project.Config{
			Validation: project.Validation{Required: cfg.Validation.Required},
		}, nil, stepDone, outputCb)
		if err != nil {
			renderer.Stop()
			return summary.finish(err)
		}
		if !valResult.RequiredPassed {
			summary.requiredFailed = true
		}
		renderer.SetStatus(reqItem, parentStatus(reqItem), "")
	}

	// Run advisory validation.
	if len(cfg.Validation.Advisory) > 0 {
		advItem := renderer.AddItem("Validation (advisory)")
		renderer.SetStatus(advItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(advItem, cfg.Validation.Advisory)
		_, err := sess.RunValidation(ctx, &project.Config{
			Validation: project.Validation{Advisory: cfg.Validation.Advisory},
		}, nil, stepDone, outputCb)
		if err != nil {
			renderer.Stop()
			return summary.finish(err)
		}
		renderer.SetStatus(advItem, parentStatus(advItem), "")
	}

	if !c.NoCache {
		item := renderer.AddItem("Saving cache")
		renderer.SetStatus(item, taskui.StatusRunning, "")

		// Save cache after all steps complete (non-fatal on error).
		if err := sess.SaveCache(ctx); err != nil {
			renderer.SetStatus(item, taskui.StatusFailed, fmt.Sprintf("failed to save cache: %v", err))
		} else {
			renderer.SetStatus(item, taskui.StatusPassed, "")
		}
	}

	shutdown()
	renderer.Stop()
	return summary.finish(nil)
}

// resolveProjectName figures out which project name to use when looking
// up secrets. The precedence is:
//  1. --project flag.
//  2. git config --get remote.origin.url in c.Dir, matched against the
//     RepoURL of entries in the project store.
//  3. Error pointing the user at `kvarn secrets set` or --project.
//
// The git lookup is best-effort: anything from a missing .git directory
// to no matching project falls through to the error path.
func (c *Cmd) resolveProjectName(ctx context.Context) (string, error) {
	if c.Project != "" {
		return c.Project, nil
	}

	remote, ok := gitOriginURL(c.Dir)
	if ok {
		store := projecttoml.New(projecttoml.DefaultPath())
		projects, err := store.List(ctx)
		if err == nil {
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

// gitOriginURL returns remote.origin.url for the repository containing
// dir, or ("", false) if git is unavailable, dir is not a repository, or
// no origin remote is set.
func gitOriginURL(dir string) (string, bool) {
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", false
	}
	return url, true
}

// openSecretStore opens the per-project secret store, surfacing a
// friendlier error than a raw file-not-found when the store has never
// been initialised.
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
					"`kvarn secrets set <project> <NAME>` before running `kvarn test`.",
				resolved)
		}
		return nil, fmt.Errorf("stat %s: %w", resolved, err)
	}
	return store, nil
}

type summaryState struct {
	passed         int
	failed         int
	skipped        int
	failedDetails  []stepOutput
	requiredFailed bool
}

type stepOutput struct {
	name   string
	stdout string
	stderr string
	err    error
}

func (s *summaryState) finish(err error) error {
	// Print details of failed steps.
	for _, step := range s.failedDetails {
		fmt.Printf("\n--- %s ---\n", step.name)
		if step.err != nil {
			fmt.Printf("  error: %v\n", step.err)
		}
		if strings.TrimSpace(step.stdout) != "" {
			for _, line := range strings.Split(strings.TrimRight(step.stdout, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
		if strings.TrimSpace(step.stderr) != "" {
			for _, line := range strings.Split(strings.TrimRight(step.stderr, "\n"), "\n") {
				fmt.Printf("    %s\n", line)
			}
		}
	}

	fmt.Printf("\n%d passed, %d failed, %d skipped\n", s.passed, s.failed, s.skipped)

	if err != nil {
		return err
	}
	if s.requiredFailed || s.failed > 0 {
		return errors.New("test failed")
	}
	return nil
}
