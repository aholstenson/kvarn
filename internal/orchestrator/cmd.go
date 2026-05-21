package orchestrator

import (
	"context"
	"log/slog"

	llms "github.com/aholstenson/llms-go"
	"github.com/cockroachdb/errors"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	credtoml "github.com/aholstenson/kvarn/internal/config/credential/tomlstore"
	forgetoml "github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	modelcfg "github.com/aholstenson/kvarn/internal/config/model"
	modeltoml "github.com/aholstenson/kvarn/internal/config/model/tomlstore"
	projtoml "github.com/aholstenson/kvarn/internal/config/project/tomlstore"
	secrettoml "github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	"github.com/aholstenson/kvarn/internal/forge"
	forgegithub "github.com/aholstenson/kvarn/internal/forge/github"
	forgegit "github.com/aholstenson/kvarn/internal/forge/git"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/local"
	"github.com/aholstenson/kvarn/internal/session"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
)

type Cmd struct {
	Addr            string `help:"Address to listen on." default:":8080"`
	DiskImagePath   string `help:"Path to VM disk image. Auto-detected if not set."`
	ProjectsFile    string `help:"Path to projects TOML file." default:""`
	CredentialsFile string `help:"Path to credentials TOML file." default:""`
	SecretsFile     string `help:"Path to per-project secrets TOML file." default:""`
	ForgesFile      string `help:"Path to forges TOML file." default:""`
	ModelsFile      string `help:"Path to models TOML file." default:""`
	Model           string `help:"LLM model alias for the coding agent." default:"coding-agent"`
}

func (c *Cmd) Run() error {
	diskImagePath := c.DiskImagePath
	if diskImagePath == "" {
		resolved, err := vm.ResolveDiskImagePath()
		if err != nil {
			return errors.Wrap(err, "find disk image")
		}
		diskImagePath = resolved
	}

	p := &local.Provider{}
	base := vm.BaseImage{
		DiskImagePath: diskImagePath,
	}

	image, err := p.PrepareImage(context.Background(), base)
	if err != nil {
		return errors.Wrap(err, "prepare image")
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

	logger := slog.Default()
	mgr, err := llms.NewManager(llms.WithManagerLogger(logger))
	if err != nil {
		return errors.Wrap(err, "create llms manager")
	}

	models, err := modelcfg.Resolve(
		context.Background(), mgr,
		modeltoml.OpenDefault(c.ModelsFile),
		coding.DefaultModels(),
		coding.ModelMain, c.Model,
	)
	if err != nil {
		return err
	}

	return run(c.Addr, ServiceOpts{
		Provider:         p,
		CreateOpts:       vm.CreateOpts{Image: image},
		ProjectStore:     projtoml.New(projectsPath),
		CredentialStore:  credtoml.New(credentialsPath),
		SecretStore:      secrettoml.New(secretsPath),
		ForgeConfigStore: forgetoml.New(forgesPath),
		ForgeTypes: map[string]forge.Forge{
			"github": forgegithub.New(),
			"git":    forgegit.New(),
		},
		SessionMgr: session.NewMemoryManager(),
		Agent:      coding.NewCodingAgent(models),
		Transferer: &transfer.StreamingTransferer{},
	})
}
