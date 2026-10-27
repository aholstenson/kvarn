package sandbox_test

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("RunValidation with prompt steps", func() {
	var (
		runner *mockRunnerProxy
		ctx    context.Context
		judged []string
		judge  sandbox.PromptJudge
		events []string
	)

	BeforeEach(func() {
		runner = newMockRunner()
		ctx = context.Background()
		judged = nil
		events = nil
		judge = func(_ context.Context, step project.Step) (sandbox.PromptVerdict, error) {
			judged = append(judged, step.Name)
			return sandbox.PromptVerdict{Passed: true}, nil
		}
	})

	onDone := func(sr sandbox.StepResult, _ string) { events = append(events, sr.Name) }

	It("runs prompt steps after the command steps and keeps the declared order in the result", func() {
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{
				{Name: "Acceptance", Prompt: "Check the export works."},
				{Name: "Tests", Run: "go test ./..."},
			},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, onDone, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(events).To(Equal([]string{"Tests", "Acceptance"}))
		Expect(result.Required).To(HaveLen(2))
		Expect(result.Required[0].Name).To(Equal("Acceptance"))
		Expect(result.Required[0].Prompt).To(BeTrue())
		Expect(result.Required[1].Name).To(Equal("Tests"))
		Expect(result.RequiredPassed).To(BeTrue())
	})

	It("skips prompt steps when a required command step fails", func() {
		runner.responses["go test ./..."] = &v1.SessionExecResponse{ExitCode: 1}
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{
				{Name: "Tests", Run: "go test ./..."},
				{Name: "Acceptance", Prompt: "Check the export works."},
			},
			Advisory: []project.Step{
				{Name: "Style", Prompt: "Check the style guide."},
			},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(judged).To(BeEmpty())
		Expect(result.RequiredPassed).To(BeFalse())
		Expect(result.Required[1].Skipped).To(BeTrue())
		Expect(result.Required[1].SkipReason).To(Equal(sandbox.SkipReasonRequiredFailed))
		Expect(result.Advisory[0].Skipped).To(BeTrue())
	})

	It("fails the required result on a failing verdict and sends the findings as output", func() {
		judge = func(_ context.Context, _ project.Step) (sandbox.PromptVerdict, error) {
			return sandbox.PromptVerdict{Passed: false, Findings: "- the button is missing"}, nil
		}
		var output string
		onOutput := func(_, _ string, stdout, _ string) { output += stdout }
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{{Name: "Acceptance", Prompt: "Check the export works."}},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, nil, onOutput)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequiredPassed).To(BeFalse())
		Expect(result.Required[0].ExitCode).To(Equal(int32(1)))
		Expect(result.Required[0].Stdout).To(Equal("- the button is missing"))
		Expect(output).To(ContainSubstring("the button is missing"))
	})

	It("stops validation when a judge could not reach a verdict", func() {
		judge = func(_ context.Context, step project.Step) (sandbox.PromptVerdict, error) {
			judged = append(judged, step.Name)
			return sandbox.PromptVerdict{}, errors.New("model unavailable")
		}
		var stderr string
		onOutput := func(_, _ string, _, e string) { stderr += e }
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{
				{Name: "Acceptance", Prompt: "Check the export works."},
				{Name: "Docs", Prompt: "Check the docs."},
			},
			Advisory: []project.Step{{Name: "Lint", Run: "npm run lint"}},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, onDone, onOutput)
		Expect(err).To(MatchError(ContainSubstring(`prompt step "Acceptance": model unavailable`)))
		Expect(judged).To(Equal([]string{"Acceptance"}), "nothing after the failed check runs")
		Expect(result.RequiredPassed).To(BeFalse())
		Expect(result.Required[0].Err).To(MatchError("model unavailable"))
		Expect(events).To(ContainElement("Acceptance"), "the step is still reported")
		Expect(stderr).To(ContainSubstring("model unavailable"))
		Expect(runner.sessionExecCalls).To(BeEmpty(), "advisory steps do not run either")
	})

	It("stops validation when an advisory prompt step could not reach a verdict", func() {
		judge = func(_ context.Context, _ project.Step) (sandbox.PromptVerdict, error) {
			return sandbox.PromptVerdict{}, errors.New("could not restore the workspace")
		}
		cfg := &project.Config{Validation: project.Validation{
			Advisory: []project.Step{{Name: "Style", Prompt: "Check the style."}},
		}}

		_, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, nil, nil)
		Expect(err).To(MatchError(ContainSubstring("could not restore the workspace")))
	})

	It("fails a step that timed out like a command that timed out, and goes on", func() {
		judge = func(_ context.Context, step project.Step) (sandbox.PromptVerdict, error) {
			judged = append(judged, step.Name)
			if step.Name == "Slow" {
				return sandbox.PromptVerdict{}, fmt.Errorf("%w (10m0s)", sandbox.ErrPromptTimeout)
			}
			return sandbox.PromptVerdict{Passed: true}, nil
		}
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{
				{Name: "Slow", Prompt: "Check the slow page."},
				{Name: "Fast", Prompt: "Check the fast page."},
			},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(judged).To(Equal([]string{"Slow", "Fast"}))
		Expect(result.RequiredPassed).To(BeFalse())
		Expect(result.Required[0].TimedOut).To(BeTrue())
		Expect(result.Required[0].Err).To(BeNil())
		Expect(result.Required[0].Stdout).To(ContainSubstring("ran past its timeout"))
	})

	It("does not fail the run on a failing advisory prompt step", func() {
		judge = func(_ context.Context, _ project.Step) (sandbox.PromptVerdict, error) {
			return sandbox.PromptVerdict{Passed: false, Findings: "- naming"}, nil
		}
		cfg := &project.Config{Validation: project.Validation{
			Advisory: []project.Step{{Name: "Style", Prompt: "Check the style guide."}},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, judge, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequiredPassed).To(BeTrue())
		Expect(result.Advisory[0].ExitCode).To(Equal(int32(1)))
	})

	It("skips prompt steps when there is no judge", func() {
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{{Name: "Acceptance", Prompt: "Check the export works."}},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", nil, nil, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Required[0].Skipped).To(BeTrue())
		Expect(result.Required[0].SkipReason).To(Equal(sandbox.SkipReasonNoJudge))
		Expect(runner.sessionExecCalls).To(BeEmpty())
	})

	It("gates prompt steps on their paths like command steps", func() {
		cfg := &project.Config{Validation: project.Validation{
			Required: []project.Step{{Name: "UI", Prompt: "Check the UI.", Paths: []string{"web/**"}}},
		}}

		result, err := sandbox.RunValidation(ctx, runner, cfg, "sess-1", []string{"cmd/main.go"}, judge, nil, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(judged).To(BeEmpty())
		Expect(result.Required[0].Skipped).To(BeTrue())
		Expect(result.Required[0].SkipReason).To(BeEmpty())
	})
})
