package jobs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/cmd/client"
	"gopkg.in/yaml.v3"
)

// StartCmd submits a new project-aware job to the orchestrator.
type StartCmd struct {
	connectFlags

	Project string `arg:"" help:"Project name."`
	Prompt  string `arg:"" help:"Prompt for the agent."`
	Branch  string `help:"Branch to start from; defaults to the project's default branch. Mutually exclusive with --pr-ref." default:""`
	PRRef   string `name:"pr-ref" help:"Continue this pull request instead of opening one: run on its head branch and push a follow-up commit. In the forge's own format (GitHub: the PR number)." default:""`
	Mode    string `help:"Agent mode: a built-in (see 'kvarn modes list') or one the project's kvarn.yml defines. Defaults to feedback with --pr-ref and auto otherwise." default:""`
	// ModeSpec is the escape hatch for a run whose shape no repository
	// defines: the definition travels with the request instead of coming out
	// of a kvarn.yml.
	ModeSpec string `name:"mode-spec" help:"Path to a YAML or JSON mode definition to run with, or - for stdin. Use instead of, or together with, --mode." default:""`
	Watch    bool   `help:"Stream the session until it reaches a terminal state." negatable:""`

	IdempotencyKey string `help:"Key that makes this submission replayable: resending it returns the session the first request created instead of starting a second job." default:""`
}

func (c *StartCmd) Run() error {
	ctx := context.Background()
	oc := c.client()

	req := &v1.StartJobRequest{
		Project: c.Project,
		Prompt:  c.Prompt,
		Mode:    c.Mode,

		IdempotencyKey: c.IdempotencyKey,
	}
	if c.ModeSpec != "" {
		spec, err := loadModeSpec(c.ModeSpec)
		if err != nil {
			return err
		}
		req.ModeSpec = spec
	}
	// Only one starting point reaches the wire. Leaving both unset is the
	// project's default branch, which is what a caller who named neither meant.
	switch {
	case c.PRRef != "" && c.Branch != "":
		return fmt.Errorf("--branch and --pr-ref are alternatives: a pull request already fixes the branch its commits land on")
	case c.PRRef != "":
		req.StartFrom = &v1.StartJobRequest_PrRef{PrRef: c.PRRef}
	case c.Branch != "":
		req.StartFrom = &v1.StartJobRequest_Branch{Branch: c.Branch}
	}

	resp, err := oc.StartJob(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}

	sessionID := resp.Msg.SessionId
	fmt.Fprintf(os.Stdout, "Session: %s\n", sessionID)
	if resp.Msg.Duplicate {
		fmt.Fprintf(os.Stdout, "Idempotency key %q already started this job; no new job was submitted.\n", c.IdempotencyKey)
	}

	if !c.Watch {
		return nil
	}

	return client.WatchSession(ctx, oc, sessionID)
}

// modeSpecFile is the file form of an inline mode definition. It is the same
// shape a `modes:` entry in kvarn.yml has, plus the name the map key would
// otherwise supply. YAML is a superset of JSON, so one decoder reads both.
type modeSpecFile struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Extends     string   `yaml:"extends"`
	Prompt      string   `yaml:"prompt"`
	Workspace   string   `yaml:"workspace"`
	Validation  string   `yaml:"validation"`
	Deliver     []string `yaml:"deliver"`
	Start       string   `yaml:"start"`
	Context     []string `yaml:"context"`
}

// loadModeSpec reads a mode definition from a file, or from stdin when path is
// "-". Unknown keys are rejected rather than dropped: a misspelled axis would
// otherwise run as a mode that silently does something else.
func loadModeSpec(path string) (*v1.ModeSpec, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read mode definition: %w", err)
	}

	var spec modeSpecFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("parse mode definition: %w", err)
	}

	// An empty list cannot survive the trip: a repeated field carries no way to
	// tell "written out empty" from "absent", so the orchestrator would read it
	// as "inherit" and deliver where the base mode delivers. Catching it here is
	// what keeps the file honest, and the message names the spelling that means
	// what the empty list looks like it means.
	if spec.Deliver != nil && len(spec.Deliver) == 0 {
		return nil, fmt.Errorf("mode definition has an empty deliver list: write deliver: [none] to deliver nothing, or omit deliver to inherit")
	}
	if spec.Context != nil && len(spec.Context) == 0 {
		return nil, fmt.Errorf("mode definition has an empty context list: write context: [none] to assemble no context, or omit context to inherit")
	}

	return &v1.ModeSpec{
		Name:        spec.Name,
		Description: spec.Description,
		Extends:     spec.Extends,
		Prompt:      spec.Prompt,
		Workspace:   spec.Workspace,
		Validation:  spec.Validation,
		Deliver:     spec.Deliver,
		Start:       spec.Start,
		Context:     spec.Context,
	}, nil
}
