package orchestrator

import (
	"context"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/session"
)

var _ = Describe("Listing jobs alongside preview boots", func() {
	var (
		ctx context.Context
		svc *Service
		mgr session.Manager
	)

	BeforeEach(func() {
		ctx = context.Background()
		mgr = session.NewManager(session.NewMemStore())
		svc = &Service{sessionMgr: mgr}

		_, err := mgr.Create(ctx, session.CreateParams{
			ProjectName: "alpha",
			Prompt:      "fix the bug",
			Mode:        "auto",
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = mgr.Create(ctx, session.CreateParams{
			ProjectName: "alpha",
			Prompt:      "Preview environment for feature/x",
			Mode:        "auto",
			Metadata:    map[string]string{previewSessionMarker: "pv-1"},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	list := func(includePreviews bool) []*v1.GetSessionResponse {
		resp, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
			IncludePreviews: includePreviews,
		}))
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return resp.Msg.Sessions
	}

	prompts := func(sessions []*v1.GetSessionResponse) []string {
		out := make([]string, 0, len(sessions))
		for _, s := range sessions {
			out = append(out, s.Prompt)
		}
		return out
	}

	It("leaves the session a preview borrowed out of the listing", func() {
		Expect(prompts(list(false))).To(Equal([]string{"fix the bug"}))
	})

	It("includes it when the caller asks for it", func() {
		Expect(prompts(list(true))).To(ConsistOf(
			"fix the bug", "Preview environment for feature/x"))
	})
})
