package orchestrator

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	projconfig "github.com/aholstenson/kvarn/internal/project"
)

// maxRequestValidationSteps caps the validation steps one submission can add.
// Each prompt step is an agent run on every attempt, so the cap bounds what a
// single request can make a job spend on checks.
const maxRequestValidationSteps = 16

// storedStep is the form a submitted validation step is kept in on the
// session. It has its own JSON shape, rather than the wire form's, because the
// stored text is compared byte for byte when an idempotency key is replayed,
// and protojson output is not guaranteed to be stable.
type storedStep struct {
	Name           string   `json:"name"`
	Run            string   `json:"run,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	Paths          []string `json:"paths,omitempty"`
	TimeoutSeconds uint32   `json:"timeout_seconds,omitempty"`
	WorkingDir     string   `json:"working_dir,omitempty"`
}

type storedValidation struct {
	Required []storedStep `json:"required,omitempty"`
	Advisory []storedStep `json:"advisory,omitempty"`
}

// validationFromProto converts and checks the validation steps a submission
// carries. A request with none yields nil, so "added nothing" stays apart from
// "added an empty block" all the way to storage.
func validationFromProto(msg *v1.Validation) (*projconfig.Validation, error) {
	if msg == nil || len(msg.GetRequired())+len(msg.GetAdvisory()) == 0 {
		return nil, nil
	}
	if n := len(msg.GetRequired()) + len(msg.GetAdvisory()); n > maxRequestValidationSteps {
		return nil, fmt.Errorf("validation has %d steps; the limit is %d", n, maxRequestValidationSteps)
	}
	v := &projconfig.Validation{
		Required: stepsFromProto(msg.GetRequired()),
		Advisory: stepsFromProto(msg.GetAdvisory()),
	}
	if err := v.Validate(); err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	return v, nil
}

func stepsFromProto(in []*v1.ValidationStep) []projconfig.Step {
	out := make([]projconfig.Step, 0, len(in))
	for _, s := range in {
		out = append(out, projconfig.Step{
			Name:       s.GetName(),
			Run:        s.GetRun(),
			Prompt:     s.GetPrompt(),
			Paths:      slices.Clone(s.GetPaths()),
			Timeout:    projconfig.Duration(time.Duration(s.GetTimeoutSeconds()) * time.Second),
			WorkingDir: s.GetWorkingDir(),
		})
	}
	return out
}

// encodeValidation is the form the steps are persisted in on the session, or
// "" when the submission added none.
func encodeValidation(v *projconfig.Validation) (string, error) {
	if v == nil {
		return "", nil
	}
	stored := storedValidation{
		Required: storeSteps(v.Required),
		Advisory: storeSteps(v.Advisory),
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return "", fmt.Errorf("encode validation: %w", err)
	}
	return string(b), nil
}

func storeSteps(in []projconfig.Step) []storedStep {
	out := make([]storedStep, 0, len(in))
	for _, s := range in {
		out = append(out, storedStep{
			Name:           s.Name,
			Run:            s.Run,
			Prompt:         s.Prompt,
			Paths:          s.Paths,
			TimeoutSeconds: s.Timeout.Seconds(),
			WorkingDir:     s.WorkingDir,
		})
	}
	return out
}

// decodeValidation reads back the steps persisted on a session.
func decodeValidation(raw string) (*projconfig.Validation, error) {
	if raw == "" {
		return nil, nil
	}
	var stored storedValidation
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, fmt.Errorf("decode stored validation: %w", err)
	}
	return &projconfig.Validation{
		Required: loadSteps(stored.Required),
		Advisory: loadSteps(stored.Advisory),
	}, nil
}

func loadSteps(in []storedStep) []projconfig.Step {
	out := make([]projconfig.Step, 0, len(in))
	for _, s := range in {
		out = append(out, projconfig.Step{
			Name:       s.Name,
			Run:        s.Run,
			Prompt:     s.Prompt,
			Paths:      s.Paths,
			Timeout:    projconfig.Duration(time.Duration(s.TimeoutSeconds) * time.Second),
			WorkingDir: s.WorkingDir,
		})
	}
	return out
}

// withRequestValidation returns the configuration validation runs with: the
// project's own, followed by the steps the submission added. The project's
// configuration is not changed, and a project with no kvarn.yml gets a
// configuration that holds only the added steps.
func withRequestValidation(cfg *projconfig.Config, extra *projconfig.Validation) *projconfig.Config {
	if extra == nil {
		return cfg
	}
	merged := projconfig.Config{}
	if cfg != nil {
		merged = *cfg
	}
	merged.Validation = projconfig.Validation{
		Required: slices.Concat(merged.Validation.Required, extra.Required),
		Advisory: slices.Concat(merged.Validation.Advisory, extra.Advisory),
	}
	return &merged
}
