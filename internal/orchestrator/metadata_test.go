package orchestrator

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/session"
)

var _ = Describe("Job metadata", func() {
	var (
		ctx context.Context
		svc *Service
		mgr session.Manager
	)

	// As in the idempotency specs: no dispatcher runs, so a submission stays in
	// the backlog and the assertions are about what StartJob recorded.
	BeforeEach(func() {
		ctx = context.Background()
		mgr = session.NewManager(session.NewMemStore())
		svc = &Service{
			sessionMgr: mgr,
			projectStore: &idempotencyProjectStore{proj: &project.Project{
				Name:          "alpha",
				RepoURL:       "/nonexistent/repo.git",
				DefaultBranch: "main",
				Forge:         "test-forge",
			}},
		}
		svc.dispatcher = newDispatcher(svc, DispatchPolicy{})
	})

	start := func(md map[string]string) (*connect.Response[v1.StartJobResponse], error) {
		return svc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
			Project:  "alpha",
			Prompt:   "fix the bug",
			Metadata: md,
		}))
	}

	Describe("recording", func() {
		It("stores the annotations and returns them on the session", func() {
			md := map[string]string{"source": "slack", "request.id": "req-7"}
			resp, err := start(md)
			Expect(err).NotTo(HaveOccurred())

			got, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Metadata).To(Equal(md))
		})

		It("leaves a submission that carried none unannotated", func() {
			resp, err := start(nil)
			Expect(err).NotTo(HaveOccurred())

			got, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Metadata).To(BeEmpty())
		})

		It("does not let the caller's map alias what was stored", func() {
			md := map[string]string{"source": "slack"}
			resp, err := start(md)
			Expect(err).NotTo(HaveOccurred())

			md["source"] = "rewritten"

			got, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Metadata).To(Equal(map[string]string{"source": "slack"}))
		})
	})

	Describe("validation", func() {
		rejects := func(md map[string]string) {
			_, err := start(md)
			ExpectWithOffset(1, connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		}

		It("accepts the punctuation callers actually use in keys", func() {
			_, err := start(map[string]string{
				"source":      "slack",
				"request.id":  "req-7",
				"team_name":   "ops",
				"acme.io/run": "42",
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects too many entries", func() {
			md := make(map[string]string, maxMetadataEntries+1)
			for i := range maxMetadataEntries + 1 {
				md[string(rune('a'+i%26))+strings.Repeat("x", i)] = "v"
			}
			rejects(md)
		})

		It("rejects an oversized key or value", func() {
			rejects(map[string]string{strings.Repeat("k", maxMetadataKeyLen+1): "v"})
			rejects(map[string]string{"k": strings.Repeat("v", maxMetadataValueLen+1)})
		})

		It("rejects a total that exceeds the byte budget", func() {
			// Each pair is individually legal; together they are not.
			md := make(map[string]string, maxMetadataEntries)
			for i := range maxMetadataEntries {
				md["key"+strings.Repeat("x", i)] = strings.Repeat("v", maxMetadataValueLen)
			}
			rejects(md)
		})

		It("rejects an empty key and one shaped like an accident", func() {
			rejects(map[string]string{"": "v"})
			rejects(map[string]string{"has space": "v"})
			rejects(map[string]string{"trailing-": "v"})
			rejects(map[string]string{".leading": "v"})
		})

		It("holds back the reserved prefix", func() {
			rejects(map[string]string{"kvarn.project": "alpha"})
		})

		It("accepts an empty value, which records the key as present", func() {
			resp, err := start(map[string]string{"dry-run": ""})
			Expect(err).NotTo(HaveOccurred())

			got, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: resp.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Metadata).To(HaveKeyWithValue("dry-run", ""))
		})
	})

	Describe("filtering", func() {
		It("returns only the sessions carrying every requested pair", func() {
			slack, err := start(map[string]string{"source": "slack", "team": "ops"})
			Expect(err).NotTo(HaveOccurred())
			_, err = start(map[string]string{"source": "jira", "team": "ops"})
			Expect(err).NotTo(HaveOccurred())
			_, err = start(nil)
			Expect(err).NotTo(HaveOccurred())

			listed, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{
				Metadata: map[string]string{"source": "slack", "team": "ops"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(listed.Msg.Sessions).To(HaveLen(1))
			Expect(listed.Msg.Sessions[0].SessionId).To(Equal(slack.Msg.SessionId))
		})

		It("lists everything when no pairs are given", func() {
			_, err := start(map[string]string{"source": "slack"})
			Expect(err).NotTo(HaveOccurred())
			_, err = start(nil)
			Expect(err).NotTo(HaveOccurred())

			listed, err := svc.ListSessions(ctx, connect.NewRequest(&v1.ListSessionsRequest{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(listed.Msg.Sessions).To(HaveLen(2))
		})
	})

	Describe("resubmission", func() {
		It("keeps the first submission's annotations on an idempotent replay", func() {
			first, err := svc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
				Project:        "alpha",
				Prompt:         "fix the bug",
				IdempotencyKey: "req-1",
				Metadata:       map[string]string{"trace": "one"},
			}))
			Expect(err).NotTo(HaveOccurred())

			// Metadata is not part of what the key claimed, so a replay whose
			// annotations differ is still the same submission rather than a
			// conflict — and the record keeps what the first request said.
			second, err := svc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
				Project:        "alpha",
				Prompt:         "fix the bug",
				IdempotencyKey: "req-1",
				Metadata:       map[string]string{"trace": "two"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Msg.SessionId).To(Equal(first.Msg.SessionId))
			Expect(second.Msg.Duplicate).To(BeTrue())

			got, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: first.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Metadata).To(Equal(map[string]string{"trace": "one"}))
		})

		It("carries the annotations onto a retry", func() {
			first, err := start(map[string]string{"source": "slack"})
			Expect(err).NotTo(HaveOccurred())
			Expect(mgr.UpdateState(ctx, first.Msg.SessionId, session.StateFailed, "boom")).To(Succeed())

			retried, err := svc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{
				SessionId: first.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(retried.Msg.SessionId).NotTo(Equal(first.Msg.SessionId))

			got, err := svc.GetSession(ctx, connect.NewRequest(&v1.GetSessionRequest{
				SessionId: retried.Msg.SessionId,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Msg.Metadata).To(Equal(map[string]string{"source": "slack"}))
		})
	})
})
