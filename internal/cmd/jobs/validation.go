package jobs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"slices"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"gopkg.in/yaml.v3"
)

// loadValidation reads validation steps from a file, or from stdin when path
// is "-". The file has the shape of the `validation:` block in kvarn.yml, so a
// step can move between the two unchanged. Unknown keys are rejected, and the
// steps are checked here with the rules the orchestrator applies, so a mistake
// is reported before the job is submitted.
func loadValidation(path string) (*v1.Validation, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read validation: %w", err)
	}

	var v project.Validation
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse validation: %w", err)
	}
	if err := v.Validate(); err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}
	// retry is a key of the shared step shape that only setup honours. The
	// wire form has no field for it, so it is refused rather than dropped.
	for _, s := range slices.Concat(v.Required, v.Advisory) {
		if s.Retry != 0 {
			return nil, fmt.Errorf("validation step %q has retry; retry applies only to setup steps", s.Name)
		}
	}

	return &v1.Validation{
		Required: validationStepsToProto(v.Required),
		Advisory: validationStepsToProto(v.Advisory),
	}, nil
}

func validationStepsToProto(steps []project.Step) []*v1.ValidationStep {
	out := make([]*v1.ValidationStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, &v1.ValidationStep{
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
