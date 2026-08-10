package orchestrator

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/cost"
	"github.com/aholstenson/kvarn/internal/config/limits"
)

var _ = Describe("comment sections", func() {
	entries := []worklogEntry{
		{kind: worklogText, text: "read the failing test"},
		{kind: worklogToolUse, toolID: "bash", args: "go test ./..."},
	}
	report := cost.Report{InputTokens: 1000, OutputTokens: 200, TotalUSD: 0.42}

	both := commentSections{worklog: true, cost: true}
	neither := commentSections{}

	Describe("sectionsFrom", func() {
		It("carries both toggles across from resolved limits", func() {
			out := sectionsFrom(limits.Limits{ReportWorklogOnPR: true, ReportCostOnPR: false})
			Expect(out.worklog).To(BeTrue())
			Expect(out.cost).To(BeFalse())
		})
	})

	Describe("formatWorklogComment", func() {
		It("includes both sections when enabled", func() {
			body := formatWorklogComment("do the thing", entries, both, report)
			Expect(body).To(ContainSubstring("<summary>Work log</summary>"))
			Expect(body).To(ContainSubstring("go test ./..."))
			Expect(body).To(ContainSubstring("## Cost"))
		})

		It("drops the work log but keeps the task when the section is off", func() {
			body := formatWorklogComment("do the thing", entries,
				commentSections{worklog: false, cost: true}, report)
			Expect(body).NotTo(ContainSubstring("Work log"))
			Expect(body).NotTo(ContainSubstring("go test ./..."))
			Expect(body).To(ContainSubstring("do the thing"))
			Expect(body).To(ContainSubstring("## Cost"))
		})

		It("drops both when neither is enabled", func() {
			body := formatWorklogComment("do the thing", entries, neither, report)
			Expect(body).NotTo(ContainSubstring("Work log"))
			Expect(body).NotTo(ContainSubstring("## Cost"))
			Expect(body).To(ContainSubstring("do the thing"))
		})
	})

	Describe("formatFollowupComment", func() {
		It("drops the work log when the section is off", func() {
			body := formatFollowupComment("please fix the lint", "fixed it", entries,
				commentSections{worklog: false, cost: true}, report)
			Expect(body).NotTo(ContainSubstring("Work log"))
			Expect(body).To(ContainSubstring("please fix the lint"))
			Expect(body).To(ContainSubstring("fixed it"))
			Expect(body).To(ContainSubstring("## Cost"))
		})
	})

	Describe("formatResultComment", func() {
		It("drops the work log while keeping the verdict", func() {
			body := formatResultComment("review this", "looks good", nil, entries,
				commentSections{worklog: false, cost: false}, report)
			Expect(body).NotTo(ContainSubstring("Work log"))
			Expect(body).NotTo(ContainSubstring("## Cost"))
			Expect(body).To(ContainSubstring("review this"))
			Expect(body).To(ContainSubstring("looks good"))
		})
	})
})
