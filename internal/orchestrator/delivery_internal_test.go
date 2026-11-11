package orchestrator

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/agent/cost"
)

var _ = Describe("delivery plan", func() {
	It("renders the new pull request comment from a stored plan with the pull request's number", func() {
		plan := deliveryPlan{
			SessionID: "s1",
			Project:   "web",
			Comment: &commentStep{
				Kind: commentNewPullRequest,
				Input: &commentInput{
					Request: "add a greeting",
					Worklog: toWorklogRecords([]worklogEntry{{kind: worklogText, text: "looked around"}}),
					Cost:    cost.Report{TotalUSD: 0.5},
					Sections: commentSectionSpec{
						Header: "Preview: {{ .PreviewURL }}",
					},
					Data: prTemplateData{SessionID: "s1", Branch: "kvarn/greeting-s1"},
					Preview: &previewSites{
						Domain: "preview.example.com",
						Sites:  []previewSite{{Name: "web", Host: "pr-{pr}.{domain}", Port: 3000}},
					},
				},
			},
		}

		// A delivery that resumes after a restart reads the plan back from
		// the store, so the comment must render from the decoded copy.
		raw, err := json.Marshal(plan)
		Expect(err).NotTo(HaveOccurred())
		var stored deliveryPlan
		Expect(json.Unmarshal(raw, &stored)).To(Succeed())

		svc := &Service{}
		body := svc.renderDeliveryComment(stored.Comment, "7", "", slog.Default())
		Expect(body).To(HavePrefix("Preview: https://pr-7.preview.example.com"))
		Expect(body).To(ContainSubstring("add a greeting"))
	})

	It("keeps the marker when the body has to be trimmed", func() {
		marker := deliveryMarker("s1")
		body := withDeliveryMarker(marker, strings.Repeat("é", maxCommentBody))

		Expect(len(body)).To(BeNumerically("<=", maxCommentBody))
		Expect(body).To(HaveSuffix(marker))
		Expect(body).To(ContainSubstring(strings.TrimSpace(truncationNote)))
	})

	It("waits longer after each failure, up to the cap", func() {
		Expect(deliveryBackoff(1)).To(Equal(30 * time.Second))
		Expect(deliveryBackoff(2)).To(Equal(time.Minute))
		Expect(deliveryBackoff(3)).To(Equal(2 * time.Minute))
		Expect(deliveryBackoff(10)).To(Equal(15 * time.Minute))
	})
})
