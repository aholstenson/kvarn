package agent_test

import (
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

var _ = Describe("BuildRetryPrompt", func() {
	It("renders failing exit-code steps with a stderr tail", func() {
		val := &sandbox.ValidationResult{
			Required: []sandbox.StepResult{
				{Name: "ok", ExitCode: 0, Stdout: "fine\n"},
				{Name: "tests", ExitCode: 1, Stderr: "FAIL: TestThing\nexpected 1, got 2\n"},
			},
		}
		out := agent.BuildRetryPrompt(val, 1, 3)
		Expect(out).To(ContainSubstring("attempt 1 of 4"))
		Expect(out).To(ContainSubstring("iterate on it"))
		Expect(out).To(ContainSubstring("Step tests exited 1"))
		Expect(out).To(ContainSubstring("FAIL: TestThing"))
		Expect(out).NotTo(ContainSubstring("Step ok"))
	})

	It("falls back to stdout when stderr is empty", func() {
		val := &sandbox.ValidationResult{
			Required: []sandbox.StepResult{
				{Name: "lint", ExitCode: 2, Stdout: "lint error in foo.go:1\n"},
			},
		}
		out := agent.BuildRetryPrompt(val, 2, 3)
		Expect(out).To(ContainSubstring("stdout:"))
		Expect(out).To(ContainSubstring("lint error in foo.go:1"))
	})

	It("surfaces RPC-level step errors distinctly", func() {
		val := &sandbox.ValidationResult{
			Required: []sandbox.StepResult{
				{Name: "tests", Err: errors.New("runner exec failed")},
			},
		}
		out := agent.BuildRetryPrompt(val, 1, 3)
		Expect(out).To(ContainSubstring("Step tests failed to run: runner exec failed"))
	})

	It("truncates very long stderr to keep the prompt bounded", func() {
		long := strings.Repeat("x", 20_000) + "\nIMPORTANT_TAIL\n"
		val := &sandbox.ValidationResult{
			Required: []sandbox.StepResult{
				{Name: "tests", ExitCode: 1, Stderr: long},
			},
		}
		out := agent.BuildRetryPrompt(val, 1, 3)
		Expect(len(out)).To(BeNumerically("<", agent.MaxRetryPromptStepBytes+1024))
		Expect(out).To(ContainSubstring("…(truncated)…"))
		Expect(out).To(ContainSubstring("IMPORTANT_TAIL"))
	})

	It("handles a nil validation result gracefully", func() {
		out := agent.BuildRetryPrompt(nil, 1, 3)
		Expect(out).To(ContainSubstring("no validation result available"))
	})

	It("reports when no failing steps are present", func() {
		val := &sandbox.ValidationResult{
			Required: []sandbox.StepResult{
				{Name: "ok", ExitCode: 0},
			},
		}
		out := agent.BuildRetryPrompt(val, 1, 3)
		Expect(out).To(ContainSubstring("no failing steps reported"))
	})
})
