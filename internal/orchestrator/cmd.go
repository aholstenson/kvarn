package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	apikeytoml "github.com/aholstenson/kvarn/internal/config/apikey/tomlstore"
	credtoml "github.com/aholstenson/kvarn/internal/config/credential/tomlstore"
	forgetoml "github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
	projtoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	"github.com/aholstenson/kvarn/internal/forge"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
	forgegithub "github.com/aholstenson/kvarn/internal/forge/github"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/local"
	llms "github.com/aholstenson/llms-go"
)

type Cmd struct {
	Addr            string `help:"Address to listen on." default:":8080"`
	DiskImagePath   string `help:"Path to VM disk image. Auto-detected if not set."`
	ProjectsFile    string `help:"Path to projects TOML file." default:""`
	CredentialsFile string `help:"Path to credentials TOML file." default:""`
	SecretsFile     string `help:"Path to per-project secrets TOML file." default:""`
	ForgesFile      string `help:"Path to forges TOML file." default:""`
	AgentsFile      string `help:"Path to agents config TOML file." default:""`
	APIKeysFile     string `help:"Path to API keys TOML file." default:""`
	NoAuth          bool   `help:"Disable API-key auth (local dev only)." env:"KVARN_NO_AUTH"`
	Model           string `help:"LLM model alias for the coding agent." default:"coding-agent"`
}

func (c *Cmd) Run() error {
	ctx := context.Background()

	downloadLogged := false
	diskImagePath, err := vm.EnsureDiskImage(ctx, vm.DownloadOpts{
		Path: c.DiskImagePath,
		Progress: func(_, total int64) {
			if !downloadLogged {
				downloadLogged = true
				slog.Info("downloading VM disk image", "total_bytes", total)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("find disk image: %w", err)
	}

	p := local.NewProvider()
	base := vm.BaseImage{
		DiskImagePath: diskImagePath,
	}

	image, err := p.PrepareImage(ctx, base)
	if err != nil {
		return fmt.Errorf("prepare image: %w", err)
	}

	projectsPath := c.ProjectsFile
	if projectsPath == "" {
		projectsPath = projtoml.DefaultPath()
	}
	credentialsPath := c.CredentialsFile
	if credentialsPath == "" {
		credentialsPath = credtoml.DefaultPath()
	}
	secretsPath := c.SecretsFile
	if secretsPath == "" {
		secretsPath = secrettoml.DefaultPath()
	}
	forgesPath := c.ForgesFile
	if forgesPath == "" {
		forgesPath = forgetoml.DefaultPath()
	}
	apiKeysPath := c.APIKeysFile
	if apiKeysPath == "" {
		apiKeysPath = apikeytoml.DefaultPath()
	}
	apiKeyStore := apikeytoml.New(apiKeysPath)

	if c.NoAuth {
		slog.Warn("API-key auth disabled — do not expose the orchestrator to untrusted networks")
	} else if keys, err := apiKeyStore.List(ctx); err != nil {
		slog.Warn("failed to read API key store; requests will be rejected until it is readable", "path", apiKeysPath, "error", err)
	} else if len(keys) == 0 {
		slog.Warn("API-key auth enabled but no keys configured; all requests will be rejected until `kvarn key create`", "path", apiKeysPath)
	}

	logger := slog.Default()
	mgr, err := llms.NewManager(llms.WithManagerLogger(logger))
	if err != nil {
		return fmt.Errorf("create llms manager: %w", err)
	}

	// One store instance serves both the named forge configs and the global
	// [defaults] block.
	forgeStore := forgetoml.New(forgesPath)

	agentsStore := modeltoml.OpenDefault(c.AgentsFile)
	models, configs, err := modelcfg.Resolve(
		ctx, mgr,
		agentsStore,
		coding.DefaultModels(),
		coding.ModelMain, c.Model,
	)
	if err != nil {
		return err
	}

	return run(c.Addr, ServiceOpts{
		Provider:           p,
		CreateOpts:         vm.CreateOpts{Image: image},
		ProjectStore:       projtoml.New(projectsPath),
		CredentialStore:    credtoml.New(credentialsPath),
		SecretStore:        secrettoml.New(secretsPath),
		ForgeConfigStore:   forgeStore,
		ForgeDefaultsStore: forgeStore,
		ForgeTypes: map[string]forge.Forge{
			"github": forgegithub.New(),
			"git":    forgegit.New(),
		},
		SessionMgr:     session.NewMemoryManager(),
		Agent:          coding.NewCodingAgent(models, configs),
		Transferer:     &transfer.StreamingTransferer{},
		DefaultsStore:  agentsStore,
		PricingManager: llms.NewPricingManager(logger),
		APIKeyStore:    apiKeyStore,
		AuthEnabled:    !c.NoAuth,
	})
}
