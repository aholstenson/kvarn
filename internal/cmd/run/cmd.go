package run

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"errors"
	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	"github.com/aholstenson/kvarn/internal/cmd/imageutil"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
	projectstore "github.com/aholstenson/kvarn/internal/config/project"
	projecttoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	"github.com/aholstenson/kvarn/internal/config/secret"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/linebuf"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/taskui"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/local"
)

// Cmd is the CLI command for running the coding agent against the local
// working directory without going through a forge.
type Cmd struct {
	Prompt string `arg:"" help:"Prompt for the coding agent."`

	Diff  bool `help:"Write a unified diff of all changes to stdout." xor:"output"`
	Apply bool `help:"Copy changed files from the VM back onto the host working directory." xor:"output"`

	DiskImagePath string `help:"Path to VM disk image. Auto-detected if not set."`
	Dir           string `help:"Project directory." default:"." type:"existingdir"`
	NoCache       bool   `help:"Disable cache persistence across runs." name:"no-cache"`
	CacheQuota    string `help:"Per-project tool-cache size limit for the LRU sweep (e.g. 5G)." name:"cache-quota" default:"5G"`
	Verbose       bool   `help:"Show all output, including from passing steps." short:"v"`
	Logs          bool   `help:"Show log output." name:"logs"`
	Project       string `help:"Project name for secret lookup. Falls back to git remote → project store if omitted." short:"p"`
	SecretsFile   string `help:"Override path to secrets store (default: ~/.config/kvarn/secrets.toml)." name:"secrets-file"`
	AgentsFile    string `help:"Override path to agents config (default: ~/.config/kvarn/agents.toml)." name:"agents-file"`
	Model         string `help:"LLM model alias for the coding agent." default:"coding-agent"`
	Mode          string `help:"Agent mode: auto, implement, fix, review, research." default:"auto"`

	MaxValidationRetries int `help:"Number of additional agent passes after a required validation failure. 0 disables retries." name:"max-validation-retries" default:"0"`
}

// Run is the kong-invoked entry point. It resolves defaults and delegates to
// the package-level run() so the core flow can be exercised by tests.
func (c *Cmd) Run() error {
	mode, err := coding.ModeByName(c.Mode)
	if err != nil {
		return err
	}

	if mode.WritesChanges() {
		if !c.Diff && !c.Apply {
			return errors.New("one of --diff or --apply must be specified")
		}
	}

	// Redirect slog to discard unless --logs is passed.
	if !c.Logs {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Resolve the coding-agent model(s) up front so we fail fast on missing
	// credentials before booting a VM.
	mgr, err := llms.NewManager(llms.WithManagerLogger(slog.Default()))
	if err != nil {
		return fmt.Errorf("create llms manager: %w", err)
	}
	models, configs, err := modelcfg.Resolve(
		ctx, mgr,
		modeltoml.OpenDefault(c.AgentsFile),
		coding.DefaultModels(),
		coding.ModelMain, c.Model,
	)
	if err != nil {
		return err
	}

	return c.runWith(ctx, runDeps{
		Provider: local.NewProvider(),
		Agent:    coding.NewCodingAgent(models, configs),
		Mode:     mode,
		Stdout:   os.Stdout,
	})
}

// runDeps groups the injectable dependencies of runWith so tests can stub
// out the VM provider and the agent.
type runDeps struct {
	Provider vm.Provider
	Agent    agent.Agent
	Mode     *coding.Mode
	Stdout   io.Writer
}

// runWith is the testable core of the command. All external entry points
// (real VM provider, real agent) are resolved by Run() and passed in.
func (c *Cmd) runWith(ctx context.Context, deps runDeps) error {
	mode := deps.Mode
	if mode == nil {
		mode = coding.ModeAuto
	}

	// Resolve (and if needed download) the disk image before the TUI starts so
	// any download progress goes to stderr without corrupting the renderer.
	diskImagePath, err := vm.EnsureDiskImage(ctx, vm.DownloadOpts{
		Path:     c.DiskImagePath,
		Progress: imageutil.NewProgress(os.Stderr, "Downloading VM image…"),
	})
	if err != nil {
		return fmt.Errorf("find disk image: %w", err)
	}

	renderer := taskui.New(deps.Stdout, c.Verbose)
	renderer.Start()

	cfg, err := project.Load(c.Dir)
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		renderer.Stop()
		return errors.New("no kvarn.yml found in project directory")
	}

	// Resolve any secrets declared in kvarn.yml. env-typed secrets become
	// real env vars in the VM; bearer-typed secrets are exposed as
	// per-job placeholders that the local egress proxy substitutes for
	// the real value just before the request leaves the host.
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

	// Repo context is best-effort: an empty struct is fine if the load fails.
	rc, err := repocontext.Load(c.Dir)
	if err != nil {
		slog.Warn("failed to load repo context", "error", err)
		rc = &repocontext.RepoContext{}
	}

	// Prepare VM image (no-op for already-prepared providers).
	img, err := deps.Provider.PrepareImage(ctx, vm.BaseImage{DiskImagePath: diskImagePath})
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("prepare image: %w", err)
	}

	skipFile, err := transfer.GitIgnoreFilter(c.Dir)
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("set up gitignore filter: %w", err)
	}

	absDir, err := filepath.Abs(c.Dir)
	if err != nil {
		renderer.Stop()
		return fmt.Errorf("resolve absolute dir: %w", err)
	}

	var cacheProvider cache.Provider
	var projectID string
	if !c.NoCache {
		fc, err := cache.DefaultFileCache()
		if err != nil {
			renderer.Stop()
			return fmt.Errorf("set up cache: %w", err)
		}
		cacheProvider = fc
		projectID = cache.ProjectID(absDir)
	}

	createOpts := vm.CreateOpts{Image: img}
	if len(bearerPlaceholders) > 0 {
		createOpts.Network.SecretInjector = egressproxy.NewPlaceholderInjector(bearerPlaceholders, slog.Default())
	}

	var lastProvisionItem *taskui.Item
	var dependenciesItem *taskui.Item
	markLastProvisionDone := func() {
		if lastProvisionItem != nil {
			renderer.SetStatus(lastProvisionItem, taskui.StatusPassed, "")
			lastProvisionItem = nil
		}
	}

	sess, err := sandbox.Start(ctx, sandbox.Opts{
		Provider:      deps.Provider,
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
	// The sandbox must outlive diff/apply extraction (which runs after the
	// renderer is stopped), so teardown is deferred to the very end and is not
	// shown as a TUI step.
	defer sess.Close()

	markLastProvisionDone()

	var summary summaryState

	makeCallbacks := func(parent *taskui.Item, steps []project.Step) (
		func(sandbox.StepResult, string),
		func(string, string, string, string),
	) {
		childItems := make(map[string]*taskui.Item, len(steps))
		stdoutBufs := make(map[string]*linebuf.Buffer, len(steps))
		stderrBufs := make(map[string]*linebuf.Buffer, len(steps))
		for _, step := range steps {
			child := renderer.AddChild(parent, step.Name)
			child.Status = taskui.StatusPending
			childItems[step.Name] = child
			stdoutBufs[step.Name] = &linebuf.Buffer{}
			stderrBufs[step.Name] = &linebuf.Buffer{}
		}

		bufFor := func(m map[string]*linebuf.Buffer, name string) *linebuf.Buffer {
			buf, ok := m[name]
			if !ok {
				buf = &linebuf.Buffer{}
				m[name] = buf
			}
			return buf
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
			for _, line := range bufFor(stdoutBufs, stepName).Append(stdout) {
				renderer.AppendOutput(item, line)
			}
			for _, line := range bufFor(stderrBufs, stepName).Append(stderr) {
				renderer.AppendOutput(item, line)
			}
		}

		stepDone := func(result sandbox.StepResult, _ string) {
			item, ok := childItems[result.Name]
			if !ok {
				item = renderer.AddChild(parent, result.Name)
				childItems[result.Name] = item
			}

			if tail := bufFor(stdoutBufs, result.Name).Flush(); tail != "" {
				renderer.AppendOutput(item, tail)
			}
			if tail := bufFor(stderrBufs, result.Name).Flush(); tail != "" {
				renderer.AppendOutput(item, tail)
			}

			if result.Skipped {
				summary.skipped++
				renderer.SetStatus(item, taskui.StatusSkipped, "(no matching files)")
			} else if result.ExitCode != 0 || result.Err != nil {
				summary.failed++
				renderer.SetStatus(item, taskui.StatusFailed, "")

				// StepResult carries the complete output from the final RPC
				// response, so the post-run dump always replays the full body
				// regardless of what the live tail was capped to.
				summary.failedDetails = append(summary.failedDetails, stepOutput{
					name:   result.Name,
					stdout: result.Stdout,
					stderr: result.Stderr,
					err:    result.Err,
				})
			} else {
				summary.passed++
				renderer.SetStatus(item, taskui.StatusPassed, "")
			}

			delete(childItems, result.Name)
			delete(stdoutBufs, result.Name)
			delete(stderrBufs, result.Name)
		}

		return stepDone, outputCb
	}

	parentStatus := func(parent *taskui.Item) taskui.Status {
		for _, child := range parent.Children {
			if child.Status == taskui.StatusFailed {
				return taskui.StatusFailed
			}
		}
		return taskui.StatusPassed
	}

	// Setup.
	if len(cfg.Setup.Steps) > 0 {
		setupItem := renderer.AddItem("Setup")
		renderer.SetStatus(setupItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(setupItem, cfg.Setup.Steps)
		if _, err := sess.RunSetup(ctx, cfg, stepDone, outputCb); err != nil {
			renderer.Stop()
			return summary.finish(deps.Stdout, err, "", 0, 0)
		}
		renderer.SetStatus(setupItem, parentStatus(setupItem), "")
	}

	// Health checks.
	if len(cfg.Setup.HealthChecks) > 0 {
		healthItem := renderer.AddItem("Health Checks")
		renderer.SetStatus(healthItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(healthItem, cfg.Setup.HealthChecks)
		if _, err := sess.RunSetup(ctx, &project.Config{
			Setup: project.Setup{HealthChecks: cfg.Setup.HealthChecks},
		}, stepDone, outputCb); err != nil {
			renderer.Stop()
			return summary.finish(deps.Stdout, err, "", 0, 0)
		}
		renderer.SetStatus(healthItem, parentStatus(healthItem), "")
	}

	// Agent. Each tool call becomes its own item that freezes with its result
	// as the next call starts, and the agent's text replies appear as notes
	// between them. The renderer commits this stream to the scrollback as it
	// goes, so a long run reads top-to-bottom as "call, output, call, output"
	// while the live view stays bounded to the in-flight tool.
	renderer.AddSection("Agent")

	branch := currentBranch(c.Dir)
	var toolCount int
	tracker := cost.NewTracker(cost.TrackerOpts{
		Pricing: llms.NewPricingManager(slog.Default()),
	})
	agentStart := time.Now()
	progress, finalizeProgress := makeProgressCallback(renderer, &toolCount)
	agentCtx := &agent.Context{
		ProjectName: filepath.Base(absDir),
		Branch:      branch,
		WorkingDir:  sess.GetWorkingDir(),
		SessionID:   sess.GetShellSessionID(),
		Prompt:      c.Prompt,
		Mode:        mode,
		Runner:      sess.GetRunner(),
		RepoContext: rc,
		OnProgress:  progress,
		Cost:        tracker,
	}

	// Drive the agent through one Conversation so a validation-failure retry
	// can append a remediation message instead of starting from scratch.
	conv, agentErr := deps.Agent.Start(ctx, agentCtx)
	if agentErr != nil {
		finalizeProgress()
		summary.agentTools = toolCount
		summary.agentDuration = time.Since(agentStart)
		summary.agentFailed = true
		renderer.Stop()
		return summary.finish(deps.Stdout, agentErr, "", 0, 0)
	}
	defer conv.Close()

	maxRetries := c.MaxValidationRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	hasRequired := mode.WritesChanges() && len(cfg.Validation.Required) > 0

	var lastValResult *sandbox.ValidationResult
	var lastAgentText string
	for attempt := 0; ; attempt++ {
		followup := ""
		if attempt > 0 {
			followup = agent.BuildRetryPrompt(lastValResult, attempt, maxRetries)
		}

		text, err := conv.Run(ctx, followup)
		if err != nil {
			agentErr = err
			summary.agentFailed = true
			break
		}
		lastAgentText = text

		if !hasRequired {
			break
		}

		changedFiles, cfErr := sess.ChangedFiles(ctx)
		if cfErr != nil {
			slog.Warn("failed to get changed files for validation gating", "error", cfErr)
		}

		label := "Validation (required)"
		if attempt > 0 {
			label = fmt.Sprintf("Validation (required) — attempt %d", attempt+1)
		}
		reqItem := renderer.AddItem(label)
		renderer.SetStatus(reqItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(reqItem, cfg.Validation.Required)
		valResult, err := sess.RunValidation(ctx, &project.Config{
			Validation: project.Validation{Required: cfg.Validation.Required},
		}, changedFiles, stepDone, outputCb)
		if err != nil {
			renderer.Stop()
			return summary.finish(deps.Stdout, err, "", 0, 0)
		}
		renderer.SetStatus(reqItem, parentStatus(reqItem), "")
		lastValResult = valResult

		if valResult.RequiredPassed {
			summary.requiredFailed = false
			break
		}
		summary.requiredFailed = true
		if attempt >= maxRetries {
			break
		}
	}
	finalizeProgress()
	summary.agentTools = toolCount
	summary.agentDuration = time.Since(agentStart)

	// Advisory validation runs once after the loop so a long required-retry
	// stretch doesn't repeat the advisory block per attempt.
	if mode.WritesChanges() && len(cfg.Validation.Advisory) > 0 {
		changedFiles, cfErr := sess.ChangedFiles(ctx)
		if cfErr != nil {
			slog.Warn("failed to get changed files for advisory validation", "error", cfErr)
		}
		advItem := renderer.AddItem("Validation (advisory)")
		renderer.SetStatus(advItem, taskui.StatusRunning, "")
		stepDone, outputCb := makeCallbacks(advItem, cfg.Validation.Advisory)
		if _, err := sess.RunValidation(ctx, &project.Config{
			Validation: project.Validation{Advisory: cfg.Validation.Advisory},
		}, changedFiles, stepDone, outputCb); err != nil {
			renderer.Stop()
			return summary.finish(deps.Stdout, err, "", 0, 0)
		}
		renderer.SetStatus(advItem, parentStatus(advItem), "")
	}

	// Summarize once the agent + validation cycle is done so the cheap
	// summary call doesn't run between retry attempts. Read-only modes use
	// the agent's final reply directly as the description.
	var agentResult *agent.Result
	if agentErr == nil {
		if mode.WritesChanges() {
			if res, err := conv.Summarize(ctx); err != nil {
				slog.Warn("failed to summarize agent run; using defaults", "error", err)
				agentResult = &agent.Result{Title: "Apply agent changes"}
			} else {
				agentResult = res
			}
		} else if lastAgentText != "" {
			agentResult = &agent.Result{Description: lastAgentText}
		}
	}

	if !c.NoCache {
		item := renderer.AddItem("Saving cache")
		renderer.SetStatus(item, taskui.StatusRunning, "")
		if err := sess.SaveCache(ctx); err != nil {
			renderer.SetStatus(item, taskui.StatusFailed, fmt.Sprintf("failed to save cache: %v", err))
		} else {
			renderer.SetStatus(item, taskui.StatusPassed, "")
		}
		// Opportunistic bounded LRU sweep (non-fatal).
		if cacheProvider != nil {
			if quota, qErr := project.ParseSize(c.CacheQuota); qErr == nil {
				if _, evErr := cacheProvider.Evict(cache.Quota{PerProjectBytes: quota}); evErr != nil {
					slog.Warn("cache eviction failed", "error", evErr)
				}
			}
		}
	}

	// Stop the renderer before emitting raw diff/apply output so it
	// doesn't fight with the TUI redraw loop.
	renderer.Stop()

	// Emit output per the user's selection. We do this even when the
	// agent or validation failed so the user can inspect what happened.
	// Read-only modes never produce changes, so diff/apply are skipped.
	var diffLineCount int
	var appliedFileCount int
	var outputErr error
	if mode.WritesChanges() {
		switch {
		case c.Diff:
			diffLineCount, outputErr = emitDiff(ctx, sess.GetRunner(), sess.GetWorkingDir(), deps.Stdout)
		case c.Apply:
			appliedFileCount, outputErr = emitApply(ctx, sess, c.Dir, deps.Stdout)
		}
	} else if c.Diff || c.Apply {
		fmt.Fprintln(deps.Stdout, "Skipping --diff/--apply: read-only mode produces no changes.")
	}

	// Print the agent's title/description as the last block before the
	// summary so there's a clear record of what the agent thinks it did.
	if agentResult != nil && (agentResult.Title != "" || agentResult.Description != "") {
		fmt.Fprintln(deps.Stdout)
		if agentResult.Title != "" {
			fmt.Fprintf(deps.Stdout, "Agent: %s\n", agentResult.Title)
		}
		if agentResult.Description != "" {
			fmt.Fprintln(deps.Stdout, agentResult.Description)
		}
	}

	printCostReport(deps.Stdout, tracker.Snapshot())

	outputMode := ""
	if mode.WritesChanges() {
		switch {
		case c.Diff:
			outputMode = "diff"
		case c.Apply:
			outputMode = "apply"
		}
	}

	// Surface the agent error in the final summary, but only if we made
	// it to the end without something more disruptive failing.
	finalErr := outputErr
	if finalErr == nil && agentErr != nil {
		finalErr = agentErr
	}
	return summary.finish(deps.Stdout, finalErr, outputMode, diffLineCount, appliedFileCount)
}

// extractor is the sandbox surface emitApply needs: a runner to classify
// changes and an ExtractChanges method to copy them back to the host.
type extractor interface {
	GetRunner() sandbox.RunnerProxy
	GetWorkingDir() string
	ExtractChanges(ctx context.Context, destDir string) error
}

// emitDiff runs `git diff HEAD` inside the VM and copies the result to out.
// Returns the number of lines emitted.
func emitDiff(ctx context.Context, runner sandbox.RunnerProxy, workdir string, out io.Writer) (int, error) {
	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "HEAD"},
		WorkingDir: workdir,
	})
	if err != nil {
		return 0, fmt.Errorf("git diff HEAD: %w", err)
	}
	if _, err := io.WriteString(out, resp.Stdout); err != nil {
		return 0, fmt.Errorf("write diff: %w", err)
	}
	if resp.Stdout == "" {
		return 0, nil
	}
	return strings.Count(resp.Stdout, "\n"), nil
}

// emitApply extracts changed files from the VM and writes them onto the
// host working directory. Returns the count of files added/modified.
func emitApply(ctx context.Context, sess extractor, destDir string, out io.Writer) (int, error) {
	added, modified, deleted, err := classifyChanges(ctx, sess.GetRunner(), sess.GetWorkingDir())
	if err != nil {
		return 0, fmt.Errorf("classify changes: %w", err)
	}
	if err := sess.ExtractChanges(ctx, destDir); err != nil {
		return 0, fmt.Errorf("extract changes: %w", err)
	}
	fmt.Fprintf(out, "Applied %d files (added %d, modified %d, removed %d)\n",
		added+modified, added, modified, deleted)
	return added + modified, nil
}

// classifyChanges runs git diff against HEAD inside the VM to count added,
// modified, and deleted files. ExtractChanges also stages all changes via
// `git add -A`, but we run our own classification first so we can report
// counts before mutating the host directory.
func classifyChanges(ctx context.Context, runner sandbox.RunnerProxy, workdir string) (added, modified, deleted int, _ error) {
	stage, err := runner.Exec(ctx, &v1.ExecRequest{
		Command: "git", Args: []string{"add", "-A"}, WorkingDir: workdir,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	if stage.ExitCode != 0 {
		return 0, 0, 0, fmt.Errorf("git add -A failed (exit %d): %s", stage.ExitCode, stage.Stderr)
	}

	resp, err := runner.Exec(ctx, &v1.ExecRequest{
		Command:    "git",
		Args:       []string{"diff", "--cached", "--name-status", "HEAD"},
		WorkingDir: workdir,
	})
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(strings.TrimRight(resp.Stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'A':
			added++
		case 'M', 'R', 'C':
			modified++
		case 'D':
			deleted++
		}
	}
	return added, modified, deleted, nil
}

// makeProgressCallback builds an OnProgress callback that renders agent
// activity as a stream of items under the "Agent" section: each ToolUse adds a
// running item (labelled with the tool and a short argument preview), the
// matching ToolResult freezes it as passed or failed, and text replies become
// dim notes in between. Each callback bumps *toolCount for the final summary.
//
// ToolUse items are queued in a FIFO per (agentID, toolID) so that when many
// calls to the same tool overlap, each ToolResult resolves the item of its
// corresponding ToolUse.
//
// The returned finalize must be called once the agent run completes. The
// underlying LLM stream does not emit a result event for a tool whose handler
// returned an error (a sub-agent dispatch that fails is the common case), so
// those items would otherwise stay "running" indefinitely; finalize resolves
// any still-pending items so nothing spins after the run is over.
func makeProgressCallback(renderer *taskui.Renderer, toolCount *int) (func(agent.ProgressEvent), func()) {
	pending := make(map[string][]*taskui.Item)
	cb := func(event agent.ProgressEvent) {
		switch e := event.(type) {
		case agent.ProgressToolUse:
			*toolCount++
			name := toolLabel(e.AgentID, e.ToolID)
			if args := shortArgs(e.ArgumentsJSON); args != "" {
				name += " " + args
			}
			item := renderer.AddItem(name)
			renderer.SetStatus(item, taskui.StatusRunning, "")
			key := toolKey(e.AgentID, e.ToolID)
			pending[key] = append(pending[key], item)
		case agent.ProgressToolResult:
			key := toolKey(e.AgentID, e.ToolID)
			q := pending[key]
			if len(q) == 0 {
				return
			}
			item := q[0]
			pending[key] = q[1:]
			if e.IsError {
				renderer.SetStatus(item, taskui.StatusFailed, "")
				if e.Result != "" {
					renderer.AppendOutput(item, firstLine(e.Result))
				}
			} else {
				renderer.SetStatus(item, taskui.StatusPassed, "")
			}
		case agent.ProgressTextMessage:
			if text := strings.TrimRight(e.Text, "\n"); strings.TrimSpace(text) != "" {
				renderer.AddNote(text)
			}
		}
	}
	finalize := func() {
		for key, q := range pending {
			for _, item := range q {
				renderer.SetStatus(item, taskui.StatusFailed, "(no result)")
			}
			delete(pending, key)
		}
	}
	return cb, finalize
}

func toolLabel(agentID, toolID string) string {
	if agentID == "" {
		return toolID
	}
	return fmt.Sprintf("[%s] %s", agentID, toolID)
}

func toolKey(agentID, toolID string) string {
	return agentID + "|" + toolID
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

// shortArgs trims tool-call arguments JSON to a single-line preview suitable
// as a taskui suffix. Long blobs are truncated so the live render stays tidy.
func shortArgs(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:79] + "…"
	}
	return s
}

// currentBranch returns the short symbolic ref for HEAD, or an empty string
// if the directory is not a git repository or git is unavailable.
func currentBranch(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// resolveProjectName mirrors `kvarn test`'s lookup: explicit flag, then a
// best-effort match against the project store via git origin URL.
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
			"`kvarn projects add` and `kvarn secrets set <project> <NAME>`.",
	)
}

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
					"`kvarn secrets set <project> <NAME>` before running `kvarn run`.",
				resolved)
		}
		return nil, fmt.Errorf("stat %s: %w", resolved, err)
	}
	return store, nil
}

// printCostReport prints a compact end-of-run cost summary. Silent when no
// spend was recorded (e.g. mock agent in tests, dry runs).
func printCostReport(out io.Writer, report cost.Report) {
	if report.InputTokens == 0 && report.OutputTokens == 0 && report.TotalUSD == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Cost: $%.4f — %d input / %d output / %d cached tokens\n",
		report.TotalUSD, report.InputTokens, report.OutputTokens, report.CachedTokens)
	if len(report.PerModel) > 1 {
		ids := make([]string, 0, len(report.PerModel))
		for id := range report.PerModel {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m := report.PerModel[id]
			fmt.Fprintf(out, "  %s: $%.4f (%d in / %d out)\n",
				m.ModelID, m.TotalUSD, m.InputTokens, m.OutputTokens)
		}
	}
}

type summaryState struct {
	passed         int
	failed         int
	skipped        int
	failedDetails  []stepOutput
	requiredFailed bool
	agentFailed    bool
	agentTools     int
	agentDuration  time.Duration
}

type stepOutput struct {
	name   string
	stdout string
	stderr string
	err    error
}

func (s *summaryState) finish(out io.Writer, err error, mode string, diffLines int, appliedFiles int) error {
	for _, step := range s.failedDetails {
		fmt.Fprintf(out, "\n--- %s ---\n", step.name)
		if step.err != nil {
			fmt.Fprintf(out, "  error: %v\n", step.err)
		}
		if strings.TrimSpace(step.stdout) != "" {
			for _, line := range strings.Split(strings.TrimRight(step.stdout, "\n"), "\n") {
				fmt.Fprintf(out, "    %s\n", line)
			}
		}
		if strings.TrimSpace(step.stderr) != "" {
			for _, line := range strings.Split(strings.TrimRight(step.stderr, "\n"), "\n") {
				fmt.Fprintf(out, "    %s\n", line)
			}
		}
	}

	var parts []string
	agentPart := "agent: ok"
	if s.agentFailed {
		agentPart = "agent: failed"
	}
	if s.agentTools > 0 {
		agentPart += fmt.Sprintf(" (%d tools)", s.agentTools)
	}
	parts = append(parts, agentPart)
	switch mode {
	case "diff":
		parts = append(parts, fmt.Sprintf("diff: %d lines", diffLines))
	case "apply":
		parts = append(parts, fmt.Sprintf("applied %d files", appliedFiles))
	}
	parts = append(parts,
		fmt.Sprintf("%d passed", s.passed),
		fmt.Sprintf("%d failed", s.failed),
		fmt.Sprintf("%d skipped", s.skipped),
	)
	fmt.Fprintf(out, "\n%s\n", strings.Join(parts, ", "))

	if err != nil {
		return err
	}
	if s.agentFailed {
		return errors.New("agent failed")
	}
	if s.requiredFailed || s.failed > 0 {
		return errors.New("validation failed")
	}

	return nil
}
