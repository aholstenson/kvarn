package orchestrator

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/cost"
	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
)

var _ = Describe("comment sections", func() {
	entries := []worklogEntry{
		{kind: worklogText, text: "read the failing test"},
		{kind: worklogToolUse, toolID: "bash", args: "go test ./..."},
	}
	report := cost.Report{InputTokens: 1000, OutputTokens: 200, TotalUSD: 0.42}

	both := commentSections{worklog: true, cost: true, quote: forgeconfig.QuoteAuto}
	neither := commentSections{quote: forgeconfig.QuoteAuto}

	// A request past either QuoteAuto threshold, so "auto" folds it away.
	longPrompt := "rework the retry loop\nand the backoff\nand the metrics\nand the docs"

	Describe("sectionsFrom", func() {
		It("carries every choice across from resolved pull-request content", func() {
			out := sectionsFrom(forgeconfig.Content{
				ReportWorklog: true,
				ReportCost:    false,
				QuoteTask:     forgeconfig.QuoteCollapsed,
			}, forgeconfig.CommentNewPullRequest, prTemplateData{}, quietLogger())
			Expect(out.worklog).To(BeTrue())
			Expect(out.cost).To(BeFalse())
			Expect(out.quote).To(Equal(forgeconfig.QuoteCollapsed))
			Expect(out.header).To(BeEmpty())
		})

		It("expands the header against the run's metadata", func() {
			out := sectionsFrom(
				forgeconfig.Content{CommentHeaders: forgeconfig.CommentHeaders{
					NewPullRequest: "**Issue:** {{ .Metadata.issue_id }} ({{ .Mode }})",
				}},
				forgeconfig.CommentNewPullRequest,
				prTemplateData{Mode: "fix", Metadata: map[string]string{"issue_id": "ENG-42"}},
				quietLogger())
			Expect(out.header).To(Equal("**Issue:** ENG-42 (fix)"))
		})

		It("takes the header configured for the kind being built", func() {
			content := forgeconfig.Content{CommentHeaders: forgeconfig.CommentHeaders{
				NewPullRequest: "opened",
				FollowUpCommit: "followed up",
				PRComment:      "reviewed",
			}}
			header := func(kind forgeconfig.CommentKind) string {
				return sectionsFrom(content, kind, prTemplateData{}, quietLogger()).header
			}

			Expect(header(forgeconfig.CommentNewPullRequest)).To(Equal("opened"))
			Expect(header(forgeconfig.CommentFollowUpCommit)).To(Equal("followed up"))
			Expect(header(forgeconfig.CommentPRComment)).To(Equal("reviewed"))
		})

		It("leaves the header empty for a kind that has none", func() {
			out := sectionsFrom(
				forgeconfig.Content{CommentHeaders: forgeconfig.CommentHeaders{NewPullRequest: "opened"}},
				forgeconfig.CommentFollowUpCommit, prTemplateData{}, quietLogger())
			Expect(out.header).To(BeEmpty(),
				"the kinds are configured separately, so one must not stand in for another")
		})

		It("leaves the header empty when the submission carried no metadata", func() {
			out := sectionsFrom(
				forgeconfig.Content{CommentHeaders: forgeconfig.CommentHeaders{
					NewPullRequest: "{{ with .Metadata.issue_id }}**Issue:** {{ . }}{{ end }}",
				}},
				forgeconfig.CommentNewPullRequest, prTemplateData{}, quietLogger())
			Expect(out.header).To(BeEmpty())
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
				commentSections{worklog: false, cost: true, quote: forgeconfig.QuoteAuto}, report)
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

		It("is empty when every section is turned off, so no comment is posted", func() {
			body := formatWorklogComment("do the thing", entries,
				commentSections{quote: forgeconfig.QuoteOff}, report)
			Expect(body).To(BeEmpty())
		})
	})

	Describe("formatFollowupComment", func() {
		It("leads with the changes and trails the feedback that prompted them", func() {
			body := formatFollowupComment("please fix the lint", "fixed it", entries, both, report)
			Expect(body).To(HavePrefix("## Changes\n\nfixed it"))
			Expect(strings.Index(body, "## Feedback addressed")).To(BeNumerically(">",
				strings.Index(body, "## Changes")))
		})

		It("drops the work log when the section is off", func() {
			body := formatFollowupComment("please fix the lint", "fixed it", entries,
				commentSections{worklog: false, cost: true, quote: forgeconfig.QuoteAuto}, report)
			Expect(body).NotTo(ContainSubstring("Work log"))
			Expect(body).To(ContainSubstring("please fix the lint"))
			Expect(body).To(ContainSubstring("fixed it"))
			Expect(body).To(ContainSubstring("## Cost"))
		})
	})

	Describe("formatResultComment", func() {
		It("leads with the result and trails the task", func() {
			body := formatResultComment("review this", "looks good", nil, entries, both, report)
			Expect(body).To(HavePrefix("## Result\n\nlooks good"))
			Expect(strings.Index(body, "## Task")).To(BeNumerically(">",
				strings.Index(body, "## Result")))
		})

		It("drops the work log while keeping the verdict", func() {
			body := formatResultComment("review this", "looks good", nil, entries,
				commentSections{worklog: false, cost: false, quote: forgeconfig.QuoteAuto}, report)
			Expect(body).NotTo(ContainSubstring("Work log"))
			Expect(body).NotTo(ContainSubstring("## Cost"))
			Expect(body).To(ContainSubstring("review this"))
			Expect(body).To(ContainSubstring("looks good"))
		})
	})

	Describe("the operator's header", func() {
		withHeader := commentSections{
			header: "**Issue:** ENG-42", worklog: true, cost: true, quote: forgeconfig.QuoteAuto,
		}

		It("leads every comment a delivery posts", func() {
			Expect(formatWorklogComment("do the thing", entries, withHeader, report)).To(
				HavePrefix("**Issue:** ENG-42\n\n"))
			Expect(formatFollowupComment("fix the lint", "fixed it", entries, withHeader, report)).To(
				HavePrefix("**Issue:** ENG-42\n\n## Changes"))
			Expect(formatResultComment("review this", "looks good", nil, entries, withHeader, report)).To(
				HavePrefix("**Issue:** ENG-42\n\n## Result"))
		})

		It("carries no heading of its own, so the operator's markdown stands as written", func() {
			body := formatWorklogComment("do the thing", entries, withHeader, report)
			Expect(body).NotTo(ContainSubstring("## **Issue:**"))
		})

		It("is reason enough to post when every other section is off", func() {
			body := formatWorklogComment("do the thing", entries,
				commentSections{header: "**Issue:** ENG-42", quote: forgeconfig.QuoteOff}, report)
			Expect(body).To(Equal("**Issue:** ENG-42"))
		})

		It("still yields no comment when it is empty and every section is off", func() {
			Expect(formatWorklogComment("do the thing", entries,
				commentSections{quote: forgeconfig.QuoteOff}, report)).To(BeEmpty())
		})
	})

	Describe("quoting the request", func() {
		quoted := func(prompt string, mode forgeconfig.QuoteMode) string {
			return formatResultComment(prompt, "looks good", nil, nil,
				commentSections{quote: mode}, cost.Report{})
		}

		It("renders a short request inline under auto", func() {
			body := quoted("do the thing", forgeconfig.QuoteAuto)
			Expect(body).To(ContainSubstring("## Task\n\n> do the thing"))
			Expect(body).NotTo(ContainSubstring("<details>"))
		})

		It("folds a long request away under auto, keeping its first line visible", func() {
			body := quoted(longPrompt, forgeconfig.QuoteAuto)
			Expect(body).To(ContainSubstring("<summary>Task — rework the retry loop</summary>"))
			Expect(body).To(ContainSubstring("> and the backoff"))
			Expect(body).NotTo(ContainSubstring("## Task"))
		})

		It("folds a short request away under collapsed", func() {
			body := quoted("do the thing", forgeconfig.QuoteCollapsed)
			Expect(body).To(ContainSubstring("<summary>Task — do the thing</summary>"))
		})

		It("renders a long request inline under full", func() {
			body := quoted(longPrompt, forgeconfig.QuoteFull)
			Expect(body).To(ContainSubstring("## Task\n\n> rework the retry loop"))
			Expect(body).NotTo(ContainSubstring("<details>"))
		})

		It("leaves the request out entirely under off", func() {
			body := quoted(longPrompt, forgeconfig.QuoteOff)
			Expect(body).NotTo(ContainSubstring("Task"))
			Expect(body).NotTo(ContainSubstring("rework the retry loop"))
		})

		It("quotes blank lines so one request stays one block", func() {
			body := quoted("first paragraph\n\nsecond paragraph", forgeconfig.QuoteAuto)
			Expect(body).To(ContainSubstring("> first paragraph\n>\n> second paragraph"))
		})

		It("escapes the summary so markup in the request cannot break the fold", func() {
			body := quoted("fix <script> handling\nin the parser\nand the linter\nand the docs",
				forgeconfig.QuoteAuto)
			Expect(body).To(ContainSubstring("<summary>Task — fix &lt;script&gt; handling</summary>"))
		})

		It("truncates a long first line in the summary", func() {
			first := strings.Repeat("x", 200)
			body := quoted(first+"\nand more\nand more\nand more", forgeconfig.QuoteAuto)
			Expect(body).To(ContainSubstring("<summary>Task — " + strings.Repeat("x", 80) + "…</summary>"))
		})
	})
})
