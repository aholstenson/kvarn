package coding_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	llms "github.com/aholstenson/llms-go"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/agent"
	"github.com/aholstenson/kvarn/internal/agent/coding"
)

// recordingCheckpointer stands in for the host half of a merge.
type recordingCheckpointer struct {
	calls []agent.CheckpointRequest
	err   error
}

func (c *recordingCheckpointer) Checkpoint(_ context.Context, req agent.CheckpointRequest) error {
	c.calls = append(c.calls, req)
	return c.err
}

// gitReplies answers the git commands the merge issues in the guest, keyed by
// the arguments joined with spaces. Anything unlisted succeeds silently, which
// is what the plumbing commands with no interesting output do.
type gitReplies map[string]*v1.ExecResponse

func mergeRunner(replies gitReplies, seen *[]string) *mockRunner {
	return &mockRunner{
		execFunc: func(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
			key := strings.Join(req.Args, " ")
			if seen != nil {
				*seen = append(*seen, key)
			}
			if resp, ok := replies[key]; ok {
				return resp, nil
			}
			return &v1.ExecResponse{}, nil
		},
	}
}

var _ = Describe("Merge tools", func() {
	var (
		ctx          context.Context
		checkpointer *recordingCheckpointer
		replies      gitReplies
		seen         []string
	)

	// mergeCmd is the merge exactly as StartMerge issues it, LFS overrides
	// included, which is the key the fake guest answers on.
	const mergeCmd = "-c filter.lfs.smudge=cat -c filter.lfs.required=false merge --no-ff --no-commit refs/heads/main"

	// The guest as a merge finds it: the base branch is not yet an ancestor, the
	// merge applies, and the commands after it report a resolved tree.
	newTools := func(target *agent.MergeTarget, cp agent.Checkpointer) map[string]llms.ToolDef {
		GinkgoHelper()
		toolkit := coding.NewCodingToolkitWithOpts(coding.CodingToolkitOpts{
			Runner:       mergeRunner(replies, &seen),
			WorkingDir:   "/home/kvarn/workspace",
			SessionID:    "sess-1",
			HeadBranch:   "feature",
			MergeTarget:  target,
			Checkpointer: cp,
		})
		tools := make(map[string]llms.ToolDef)
		for _, t := range toolkit.Tools() {
			tools[t.Name()] = t
		}
		return tools
	}

	BeforeEach(func() {
		ctx = context.Background()
		checkpointer = &recordingCheckpointer{}
		seen = nil
		replies = gitReplies{
			"merge-base --is-ancestor refs/heads/main HEAD": {ExitCode: 1},
			"rev-parse --verify --quiet MERGE_HEAD":         {ExitCode: 0, Stdout: "aaa\n"},
			"rev-parse HEAD":                                {ExitCode: 0, Stdout: "merge-sha\n"},
		}
	})

	It("is not offered without both a target and a checkpointer", func() {
		Expect(newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, nil)).
			NotTo(HaveKey("merge_branch"))
		Expect(newTools(nil, checkpointer)).NotTo(HaveKey("finish_merge"))
	})

	It("merges the run's base branch", func() {
		tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)

		out, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.(*coding.MergeBranchOutput).Conflicts).To(BeEmpty())
		Expect(seen).To(ContainElement(ContainSubstring("merge --no-ff --no-commit refs/heads/main")))
		Expect(tools["merge_branch"].Render(out).Text).To(ContainSubstring("finish_merge"))
	})

	It("rejects a branch other than the target", func() {
		tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)

		_, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{Branch: "develop"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(`only "main" can be merged`))
		Expect(seen).NotTo(ContainElement(ContainSubstring("merge --no-ff")))
	})

	It("refuses a second merge in the same run", func() {
		tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)

		_, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
		Expect(err).NotTo(HaveOccurred())

		_, err = tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("already been made"))
	})

	It("leaves the run free to merge when the branch is already in", func() {
		replies["merge-base --is-ancestor refs/heads/main HEAD"] = &v1.ExecResponse{ExitCode: 0}
		tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)

		out, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
		Expect(err).NotTo(HaveOccurred())
		Expect(out.(*coding.MergeBranchOutput).AlreadyUpToDate).To(BeTrue())

		// Nothing was started, so nothing is spent: a later call still works.
		_, err = tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("finish_merge", func() {
		It("checkpoints once, with the commit that was merged", func() {
			tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)
			_, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
			Expect(err).NotTo(HaveOccurred())

			out, err := tools["finish_merge"].Execute(ctx, &coding.FinishMergeInput{
				Notes: "kept both sides of the config",
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(checkpointer.calls).To(HaveLen(1))
			req := checkpointer.calls[0]
			Expect(req.Kind).To(Equal(agent.CheckpointMerge))
			Expect(req.MergeParent).To(Equal("base-sha"))
			Expect(req.GuestCommit).To(Equal("merge-sha"))
			Expect(req.Message).To(HavePrefix("Merge branch 'main' into 'feature'"))
			Expect(req.Message).To(ContainSubstring("kept both sides of the config"))

			Expect(out.(*coding.FinishMergeOutput).Commit).To(Equal("merge-sha"))

			// A second call adds nothing: one merge, one commit.
			_, err = tools["finish_merge"].Execute(ctx, &coding.FinishMergeInput{})
			Expect(err).To(HaveOccurred())
			Expect(checkpointer.calls).To(HaveLen(1))
		})

		It("names the resolved paths in the commit message", func() {
			replies[mergeCmd] = &v1.ExecResponse{ExitCode: 1}
			replies["diff --name-only --diff-filter=U"] = &v1.ExecResponse{Stdout: "app/config.yml\n"}

			tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)
			out, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
			Expect(err).NotTo(HaveOccurred())
			Expect(out.(*coding.MergeBranchOutput).Conflicts).To(Equal([]string{"app/config.yml"}))

			// Resolved by the time the merge is finished, so nothing is unmerged.
			replies["diff --name-only --diff-filter=U"] = &v1.ExecResponse{}

			_, err = tools["finish_merge"].Execute(ctx, &coding.FinishMergeInput{})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkpointer.calls[0].Message).To(ContainSubstring("Resolved conflicts:\n- app/config.yml"))
		})

		It("refuses before a merge has been started", func() {
			tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)

			_, err := tools["finish_merge"].Execute(ctx, &coding.FinishMergeInput{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no merge is in progress"))
			Expect(checkpointer.calls).To(BeEmpty())
		})

		It("does not report a merge the guest refused to commit", func() {
			replies["rev-parse --verify --quiet MERGE_HEAD"] = &v1.ExecResponse{ExitCode: 1}

			tools := newTools(&agent.MergeTarget{Branch: "main", SHA: "base-sha"}, checkpointer)
			_, err := tools["merge_branch"].Execute(ctx, &coding.MergeBranchInput{})
			Expect(err).NotTo(HaveOccurred())

			_, err = tools["finish_merge"].Execute(ctx, &coding.FinishMergeInput{})
			Expect(err).To(HaveOccurred())
			Expect(checkpointer.calls).To(BeEmpty())
		})
	})
})
