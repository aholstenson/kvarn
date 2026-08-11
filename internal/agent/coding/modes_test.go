package coding_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/coding"
	"github.com/aholstenson/kvarn/internal/agent/repocontext"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

var _ = Describe("ModeByName", func() {
	It("returns ModeAuto for empty input", func() {
		m, err := coding.ModeByName("")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeAuto))
	})

	It("returns ModeAuto for 'auto'", func() {
		m, err := coding.ModeByName("auto")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeAuto))
	})

	It("returns ModeImplement for 'implement'", func() {
		m, err := coding.ModeByName("implement")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeImplement))
	})

	It("returns ModeFix for 'fix'", func() {
		m, err := coding.ModeByName("fix")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeFix))
	})

	It("returns ModeFeedback for 'feedback'", func() {
		m, err := coding.ModeByName("feedback")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeFeedback))
	})

	It("returns ModeReview for 'review'", func() {
		m, err := coding.ModeByName("review")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeReview))
	})

	It("returns ModeResearch for 'research'", func() {
		m, err := coding.ModeByName("research")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeResearch))
	})

	It("is case-insensitive and trims whitespace", func() {
		m, err := coding.ModeByName("  REVIEW  ")
		Expect(err).NotTo(HaveOccurred())
		Expect(m).To(Equal(coding.ModeReview))
	})

	It("returns an error for an unknown mode", func() {
		_, err := coding.ModeByName("bogus")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("bogus"))
	})

	It("returns an error for the removed 'deliberate' mode", func() {
		_, err := coding.ModeByName("deliberate")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Mode.WritesChanges", func() {
	It("is true for auto", func() {
		Expect(coding.ModeAuto.WritesChanges()).To(BeTrue())
	})

	It("is true for implement", func() {
		Expect(coding.ModeImplement.WritesChanges()).To(BeTrue())
	})

	It("is true for fix", func() {
		Expect(coding.ModeFix.WritesChanges()).To(BeTrue())
	})

	It("is true for feedback", func() {
		Expect(coding.ModeFeedback.WritesChanges()).To(BeTrue())
	})

	It("is false for review", func() {
		Expect(coding.ModeReview.WritesChanges()).To(BeFalse())
	})

	It("is false for research", func() {
		Expect(coding.ModeResearch.WritesChanges()).To(BeFalse())
	})
})

var _ = Describe("Mode axes", func() {
	It("derives WritesChanges from the delivery sinks", func() {
		Expect(coding.ModeAuto.WritesChanges()).To(BeTrue(), "opens a pull request")
		Expect(coding.ModeFeedback.WritesChanges()).To(BeTrue(), "pushes a follow-up commit")
		Expect(coding.ModeReview.WritesChanges()).To(BeFalse(), "delivers nowhere")
	})

	It("gives read-only modes the read-only workspace", func() {
		Expect(coding.ModeReview.ReadOnly()).To(BeTrue())
		Expect(coding.ModeResearch.ReadOnly()).To(BeTrue())
		Expect(coding.ModeImplement.ReadOnly()).To(BeFalse())
	})

	It("runs validation for write modes and skips it for read-only ones", func() {
		Expect(coding.ModeImplement.Validation).To(Equal(coding.ValidationRun))
		Expect(coding.ModeReview.Validation).To(Equal(coding.ValidationSkip))
	})

	It("restricts feedback to an existing pull request", func() {
		Expect(coding.ModeFeedback.Start).To(Equal(coding.StartPullRequest))
		Expect(coding.ModeAuto.Start).To(Equal(coding.StartAny))
	})
})

var _ = Describe("SystemPrompt", func() {
	It("frames the mode's body with the role and environment block", func() {
		prompt := coding.ModeReview.SystemPrompt("acme", "git@example.com:acme/web.git", "main", nil, nil, forgeconfig.Content{})
		Expect(prompt).To(HavePrefix("You are Kvarn, a read-only code review agent running in a sandboxed VM."))
		Expect(prompt).To(ContainSubstring("- Project: acme"))
		Expect(prompt).To(ContainSubstring("- Repository: git@example.com:acme/web.git"))
		Expect(prompt).To(ContainSubstring("- Branch: main"))
		Expect(prompt).To(ContainSubstring("## Operating principles"))
	})

	It("gives a write mode the editing rules and a read-only mode none", func() {
		Expect(coding.ModeImplement.SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})).To(ContainSubstring("## Editing rules"))
		Expect(coding.ModeReview.SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})).NotTo(ContainSubstring("## Editing rules"))
	})
})

var _ = Describe("Registry", func() {
	Describe("Merge", func() {
		It("returns the built-ins when nothing is defined", func() {
			reg, err := coding.Merge(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(reg.Names()).To(ConsistOf("auto", "implement", "fix", "feedback", "review", "research"))
		})

		It("inherits every unset axis from the mode it extends", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"audit": {Extends: "review"},
			})
			Expect(err).NotTo(HaveOccurred())

			m := reg["audit"]
			Expect(m.Name).To(Equal("audit"))
			Expect(m.BaseName).To(Equal("review"), "metrics report the built-in it derives from")
			Expect(m.Workspace).To(Equal(coding.WorkspaceReadOnly))
			Expect(m.Deliver).To(Equal([]coding.Sink{coding.SinkNone}))
			Expect(m.Validation).To(Equal(coding.ValidationSkip), "derived from the inherited workspace")
			Expect(m.Start).To(Equal(coding.StartAny), "inherited from review")
		})

		It("inherits a narrower start point rather than widening it to any", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"revise": {Extends: "feedback"},
			})
			Expect(err).NotTo(HaveOccurred())

			m := reg["revise"]
			Expect(m.Deliver).To(Equal([]coding.Sink{coding.SinkFollowUpCommit}))
			Expect(m.Start).To(Equal(coding.StartPullRequest),
				"a mode that commits onto a pull request cannot start without one")
		})

		It("clears the inherited context pack when the definition asks for none", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"blind": {Extends: "feedback", Context: []coding.ContextBlock{coding.ContextNone}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(reg["blind"].Context).To(BeEmpty())
			Expect(reg["blind"].BuildPrompt(coding.ContextInput{
				PRTitle: "feat: add retries",
				Task:    "Use exponential backoff.",
			})).To(Equal("Use exponential backoff."))
		})

		It("overrides only the axes the definition names", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"review-pr": {
					Extends: "review",
					Start:   coding.StartPullRequest,
					Deliver: []coding.Sink{coding.SinkPRComment},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			m := reg["review-pr"]
			Expect(m.Workspace).To(Equal(coding.WorkspaceReadOnly), "inherited")
			Expect(m.Start).To(Equal(coding.StartPullRequest))
			Expect(m.Deliver).To(Equal([]coding.Sink{coding.SinkPRComment}))
			Expect(m.WritesChanges()).To(BeFalse(), "a comment is not a commit")
		})

		It("derives validation from an overridden workspace", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"bump": {Extends: "review", Workspace: coding.WorkspaceReadWrite,
					Deliver: []coding.Sink{coding.SinkNewPullRequest}},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(reg["bump"].Validation).To(Equal(coding.ValidationRun))
		})

		It("resolves a chain of definitions regardless of map order", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"grandchild": {Extends: "child"},
				"child":      {Extends: "review", Start: coding.StartPullRequest},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(reg["grandchild"].Workspace).To(Equal(coding.WorkspaceReadOnly))
			Expect(reg["grandchild"].BaseName).To(Equal("review"))
		})

		It("refuses to redefine a built-in", func() {
			_, err := coding.Merge(map[string]coding.Spec{"review": {}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("built in"))
		})

		It("refuses an unknown parent", func() {
			_, err := coding.Merge(map[string]coding.Spec{"a": {Extends: "nonesuch"}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nonesuch"))
		})

		It("refuses a cycle", func() {
			_, err := coding.Merge(map[string]coding.Spec{
				"a": {Extends: "b"},
				"b": {Extends: "a"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cycle"))
		})

		It("refuses a spec whose name disagrees with its key", func() {
			_, err := coding.Merge(map[string]coding.Spec{"a": {Name: "b"}})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("different name"))
		})
	})

	Describe("prompt inheritance", func() {
		It("appends the definition's prompt to the inherited body", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"audit": {Extends: "review", Prompt: "Check the egress allowlist."},
			})
			Expect(err).NotTo(HaveOccurred())

			prompt := reg["audit"].SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})
			base := coding.ModeReview.SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})
			Expect(prompt).To(ContainSubstring("## Operating principles"), "the inherited body survives")
			Expect(prompt).To(ContainSubstring("## Additional instructions\n\nCheck the egress allowlist."))
			Expect(len(prompt)).To(BeNumerically(">", len(base)), "it adds rather than replaces")
		})

		It("keeps the shared trailer after the appended prompt", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"audit": {Extends: "review", Prompt: "Check the egress allowlist."},
			})
			Expect(err).NotTo(HaveOccurred())

			prompt := reg["audit"].SystemPrompt("p", "r", "b",
				&repocontext.RepoContext{Instructions: "House rules."}, nil, forgeconfig.Content{})
			Expect(prompt).To(ContainSubstring("Check the egress allowlist."))
			Expect(strings.Index(prompt, "## Project Instructions")).To(
				BeNumerically(">", strings.Index(prompt, "Check the egress allowlist.")))
		})

		It("accumulates prompts down a chain", func() {
			reg, err := coding.Merge(map[string]coding.Spec{
				"a": {Extends: "review", Prompt: "First."},
				"b": {Extends: "a", Prompt: "Second."},
			})
			Expect(err).NotTo(HaveOccurred())
			prompt := reg["b"].SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})
			Expect(prompt).To(ContainSubstring("First."))
			Expect(prompt).To(ContainSubstring("Second."))
			Expect(strings.Index(prompt, "Second.")).To(BeNumerically(">", strings.Index(prompt, "First.")))
		})
	})

	Describe("Resolve", func() {
		It("looks a bare name up", func() {
			reg := coding.Builtins()
			m, err := reg.Resolve("review", nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(m).To(Equal(coding.ModeReview))
		})

		It("builds an inline spec against a project-defined parent", func() {
			reg, err := coding.Merge(map[string]coding.Spec{"house": {Extends: "review", Prompt: "House rules."}})
			Expect(err).NotTo(HaveOccurred())

			m, err := reg.Resolve("", &coding.Spec{Extends: "house", Deliver: []coding.Sink{coding.SinkPRComment}})
			Expect(err).NotTo(HaveOccurred())
			Expect(m.Name).To(Equal("inline"))
			Expect(m.Deliver).To(Equal([]coding.Sink{coding.SinkPRComment}))
			Expect(m.SystemPrompt("p", "r", "b", nil, nil, forgeconfig.Content{})).To(ContainSubstring("House rules."))
		})

		It("refuses an inline spec that shadows a built-in", func() {
			_, err := coding.Builtins().Resolve("review", &coding.Spec{Extends: "review"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("built in"))
		})
	})
})

var _ = Describe("MetricsModeLabel", func() {
	It("passes a built-in name through", func() {
		Expect(coding.MetricsModeLabel("review")).To(Equal("review"))
		Expect(coding.MetricsModeLabel("")).To(Equal("auto"))
	})

	It("collapses anything else to bound label cardinality", func() {
		Expect(coding.MetricsModeLabel("review-pr")).To(Equal("custom"))
	})
})

var _ = Describe("Spec.Validate", func() {
	DescribeTable("rejects an incoherent definition",
		func(spec coding.Spec, wantErr string) {
			err := spec.Validate()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantErr))
		},
		Entry("unknown workspace", coding.Spec{Workspace: "read-mostly"}, "workspace"),
		Entry("unknown validation", coding.Spec{Validation: "maybe"}, "validation"),
		Entry("unknown start", coding.Spec{Start: "tag"}, "start"),
		Entry("unknown sink", coding.Spec{Deliver: []coding.Sink{"email"}}, "deliver"),
		Entry("unknown context block", coding.Spec{Context: []coding.ContextBlock{"weather"}}, "context"),
		Entry("none plus another sink",
			coding.Spec{Deliver: []coding.Sink{coding.SinkNone, coding.SinkPRComment}}, "cannot be combined"),
		Entry("none plus another context block",
			coding.Spec{Context: []coding.ContextBlock{coding.ContextNone, coding.ContextPRDiff}}, "cannot be combined"),
		Entry("both commit sinks",
			coding.Spec{Deliver: []coding.Sink{coding.SinkFollowUpCommit, coding.SinkNewPullRequest}}, "alternatives"),
		Entry("read-only commit sink",
			coding.Spec{Workspace: coding.WorkspaceReadOnly, Deliver: []coding.Sink{coding.SinkNewPullRequest}}, "read-write"),
		Entry("branch-only follow-up commit",
			coding.Spec{Start: coding.StartBranch, Deliver: []coding.Sink{coding.SinkFollowUpCommit}}, "start"),
		Entry("bad name", coding.Spec{Name: "Review PR"}, "lowercase alphanumerics"),
	)

	It("accepts a definition that names nothing", func() {
		Expect(coding.Spec{}.Validate()).To(Succeed())
	})
})

var _ = Describe("Mode.BuildPrompt", func() {
	It("returns the task alone for a mode with no context blocks", func() {
		Expect(coding.ModeReview.BuildPrompt(coding.ContextInput{
			Task:    "  Audit the auth package.  ",
			PRTitle: "Ignored",
		})).To(Equal("Audit the auth package."))
	})

	It("assembles the feedback pack in order", func() {
		got := coding.ModeFeedback.BuildPrompt(coding.ContextInput{
			OriginalTask: "Add retries.",
			PRTitle:      "feat: add retries",
			PRBody:       "Retries the request twice.",
			PRDiff:       "+retry()",
			Task:         "Use exponential backoff.",
		})
		Expect(got).To(Equal("## Original task\n\nAdd retries.\n\n" +
			"## Current pull request\n\nfeat: add retries\n\nRetries the request twice.\n" +
			"\n## Diff\n\n```diff\n+retry()\n```\n" +
			"\n## Feedback to address\n\nUse exponential backoff."))
	})

	It("leaves out a block whose content is unavailable", func() {
		got := coding.ModeFeedback.BuildPrompt(coding.ContextInput{
			PRTitle: "feat: add retries",
			Task:    "Use exponential backoff.",
		})
		Expect(got).NotTo(ContainSubstring("## Original task"))
		Expect(got).NotTo(ContainSubstring("## Diff"))
		Expect(got).To(ContainSubstring("## Current pull request"))
	})

	It("titles the last section Task for a mode that is not addressing feedback", func() {
		reg, err := coding.Merge(map[string]coding.Spec{
			"review-pr": {Extends: "review", Start: coding.StartPullRequest,
				Deliver: []coding.Sink{coding.SinkPRComment},
				Context: []coding.ContextBlock{coding.ContextPRDiff}},
		})
		Expect(err).NotTo(HaveOccurred())

		got := reg["review-pr"].BuildPrompt(coding.ContextInput{PRDiff: "+x", Task: "Review it."})
		Expect(got).To(ContainSubstring("## Task\n\nReview it."))
	})
})
