package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/session"
)

// idempotencyProjectStore is the smallest project.Store that satisfies
// submission: one project, resolved by name.
type idempotencyProjectStore struct {
	proj *project.Project
}

func (s *idempotencyProjectStore) Get(_ context.Context, name string) (*project.Project, error) {
	if name != s.proj.Name {
		return nil, fmt.Errorf("project %q not found", name)
	}
	return s.proj, nil
}

func (s *idempotencyProjectStore) List(context.Context) ([]*project.Project, error) {
	return []*project.Project{s.proj}, nil
}
func (s *idempotencyProjectStore) Put(context.Context, *project.Project) error { return nil }
func (s *idempotencyProjectStore) Delete(context.Context, string) error        { return nil }

var _ = Describe("StartJob idempotency", func() {
	var (
		ctx context.Context
		svc *Service
		mgr session.Manager
	)

	// The service is built without a running dispatcher, so a submission stays
	// in the backlog and the specs assert on what StartJob recorded rather than
	// racing a run that would clone and boot a VM.
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

	start := func(key string) (*connect.Response[v1.StartJobResponse], error) {
		return svc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
			Project:        "alpha",
			Prompt:         "fix the bug",
			IdempotencyKey: key,
		}))
	}

	It("returns the first session when the same key is replayed", func() {
		first, err := start("req-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(first.Msg.Duplicate).To(BeFalse())

		second, err := start("req-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Msg.SessionId).To(Equal(first.Msg.SessionId))
		Expect(second.Msg.Duplicate).To(BeTrue())

		// The point of the key: exactly one job exists to be dispatched.
		pending, err := mgr.CountPending(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(Equal(1))
	})

	It("matches a replay that omits the branch the first request defaulted to", func() {
		first, err := start("req-1")
		Expect(err).NotTo(HaveOccurred())

		second, err := svc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
			Project:        "alpha",
			Prompt:         "fix the bug",
			StartFrom:      &v1.StartJobRequest_Branch{Branch: "main"},
			IdempotencyKey: "req-1",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Msg.SessionId).To(Equal(first.Msg.SessionId))
		Expect(second.Msg.Duplicate).To(BeTrue())
	})

	It("starts separate jobs for different keys", func() {
		first, err := start("req-1")
		Expect(err).NotTo(HaveOccurred())
		second, err := start("req-2")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Msg.SessionId).NotTo(Equal(first.Msg.SessionId))
		Expect(second.Msg.Duplicate).To(BeFalse())
	})

	It("starts separate jobs when no key is supplied", func() {
		first, err := start("")
		Expect(err).NotTo(HaveOccurred())
		second, err := start("")
		Expect(err).NotTo(HaveOccurred())
		Expect(second.Msg.SessionId).NotTo(Equal(first.Msg.SessionId))
		Expect(second.Msg.Duplicate).To(BeFalse())
	})

	It("refuses a key reused for a different submission", func() {
		_, err := start("req-1")
		Expect(err).NotTo(HaveOccurred())

		_, err = svc.StartJob(ctx, connect.NewRequest(&v1.StartJobRequest{
			Project:        "alpha",
			Prompt:         "something else entirely",
			IdempotencyKey: "req-1",
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeAlreadyExists))
	})

	It("rejects an oversized key", func() {
		_, err := start(strings.Repeat("k", maxIdempotencyKeyLen+1))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("creates one job when concurrent requests carry the same key", func() {
		const callers = 8
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			ids []string
		)
		for range callers {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				resp, err := start("req-race")
				Expect(err).NotTo(HaveOccurred())
				mu.Lock()
				ids = append(ids, resp.Msg.SessionId)
				mu.Unlock()
			}()
		}
		wg.Wait()

		for _, id := range ids {
			Expect(id).To(Equal(ids[0]))
		}
		pending, err := mgr.CountPending(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(Equal(1))
	})

	It("does not carry the key onto a retry of the finished job", func() {
		first, err := start("req-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(mgr.UpdateState(ctx, first.Msg.SessionId, session.StateFailed, "boom")).To(Succeed())

		retried, err := svc.RetryJob(ctx, connect.NewRequest(&v1.RetryJobRequest{
			SessionId: first.Msg.SessionId,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(retried.Msg.SessionId).NotTo(Equal(first.Msg.SessionId))

		// The retry claimed nothing, so the original key still resolves to the
		// session that took it.
		found, err := mgr.FindByIdempotencyKey(ctx, "alpha", "req-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(found.ID).To(Equal(first.Msg.SessionId))
	})

	It("reports a store conflict it cannot resolve rather than inventing a session", func() {
		// A store that claims the key is taken but never hands the session back
		// is broken; the request must fail rather than report success for a job
		// nobody can name.
		svc.sessionMgr = &conflictingManager{Manager: mgr}
		_, err := start("req-1")
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
	})
})

// conflictingManager always reports the idempotency key as taken and then finds
// nothing holding it.
type conflictingManager struct {
	session.Manager
}

func (m *conflictingManager) Create(context.Context, session.CreateParams) (*session.Session, error) {
	return nil, session.ErrIdempotencyConflict
}

func (m *conflictingManager) FindByIdempotencyKey(context.Context, string, string) (*session.Session, error) {
	return nil, nil
}
