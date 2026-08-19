package orchestrator

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	projcfg "github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/preview"
)

// fakeResolver stands in for the forge lookup behind auto-start, counting how
// often it is asked so the single-flight and the negative cache can be seen
// working rather than inferred.
type fakeResolver struct {
	mu     sync.Mutex
	calls  int
	target previewTarget
	err    error
	// block, when non-nil, holds each resolution until it is closed.
	block chan struct{}
}

func (f *fakeResolver) resolve(_ context.Context, _ string) (previewTarget, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	target, err := f.target, f.err
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	return target, err
}

func (f *fakeResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeResolver) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

var _ = Describe("Preview auto-start", func() {
	const host = "pr-12.preview.example.com"

	var (
		ctx      context.Context
		store    preview.Store
		clock    *fakePreviewClock
		booter   *fakeBooter
		resolver *fakeResolver
		mgr      *previewManager
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = preview.NewMemStore()
		clock = newPreviewClock()
		booter = newFakeBooter()
		resolver = &fakeResolver{target: previewTarget{Project: "acme", Ref: "feature/login", PR: "12"}}

		mgr = newPreviewManager(store, PreviewPolicy{Domain: "preview.example.com"}, booter.boot)
		mgr.now = clock.Now
		mgr.auto = newAutoStarter(resolver.resolve, clock.Now)
		DeferCleanup(func() { Expect(store.Close()).To(Succeed()) })
	})

	Describe("bringing a preview into being", func() {
		It("registers the pull request's head branch under the name that asked for it", func() {
			p, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.ID).To(Equal(preview.ID("acme", "feature/login")))
			Expect(p.PR).To(Equal("12"))
			Expect(p.AutoStartHost).To(Equal(host))
			Expect(p.AutoStarted()).To(BeTrue())
			// Registering is not starting: the boot is the caller's next step.
			Expect(p.State).To(Equal(preview.StateStopped))
			Expect(booter.bootCount(p.ID)).To(Equal(0))
		})

		It("normalizes the hostname it was asked about", func() {
			p, err := mgr.AutoStart(ctx, "PR-12.Preview.Example.Com:8080")
			Expect(err).NotTo(HaveOccurred())
			Expect(p.AutoStartHost).To(Equal(host))
		})

		It("joins a preview that already exists for the ref", func() {
			first, err := mgr.Register(ctx, "acme", "feature/login", previewOrigin{})
			Expect(err).NotTo(HaveOccurred())
			Expect(first.PR).To(BeEmpty())

			second, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			Expect(second.ID).To(Equal(first.ID))
			// The hostname taught the existing record which pull request it is.
			Expect(second.PR).To(Equal("12"))
			// It did not take the record over. An operator registered this one,
			// so merging the pull request must not delete it.
			Expect(second.AutoStartHost).To(BeEmpty())
			Expect(second.AutoStarted()).To(BeFalse())
		})

		It("reports a hostname the resolver claims nothing for", func() {
			resolver.setErr(preview.ErrNoRoute)
			_, err := mgr.AutoStart(ctx, "www.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNoRoute))
		})

		It("does nothing on a manager with no auto-start wired up", func() {
			plain := newPreviewManager(store, PreviewPolicy{Domain: "preview.example.com"}, booter.boot)
			_, err := plain.AutoStart(ctx, host)
			Expect(err).To(MatchError(preview.ErrNoRoute))
		})
	})

	Describe("guarding the resolver", func() {
		It("resolves a hostname once for a burst of requests", func() {
			// A browser opening a preview asks for a page and then every asset
			// on it. Each of those would otherwise be its own forge call.
			resolver.block = make(chan struct{})

			var wg sync.WaitGroup
			for range 10 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()
					_, err := mgr.AutoStart(ctx, host)
					Expect(err).NotTo(HaveOccurred())
				}()
			}

			Eventually(resolver.count).Should(Equal(1))
			Consistently(resolver.count).Should(Equal(1))
			close(resolver.block)
			wg.Wait()
			Expect(resolver.count()).To(Equal(1))
		})

		It("remembers what a hostname resolved to", func() {
			// The hostname keeps arriving here until the boot claims it, and a
			// first boot is minutes while the holding page polls every couple of
			// seconds. Re-asking the forge each time spends a rate-limit budget
			// the whole host shares.
			for range 5 {
				p, err := mgr.AutoStart(ctx, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(p.PR).To(Equal("12"))
			}
			Expect(resolver.count()).To(Equal(1))
		})

		It("asks again once the remembered answer has gone stale", func() {
			_, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())

			clock.advance(previewResolvedTTL + time.Second)

			_, err = mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolver.count()).To(Equal(2))
		})

		It("remembers that a hostname resolved to nothing", func() {
			resolver.setErr(errors.New("pull request 12 is closed"))

			for range 5 {
				_, err := mgr.AutoStart(ctx, host)
				Expect(err).To(MatchError(ContainSubstring("is closed")))
			}
			Expect(resolver.count()).To(Equal(1))
		})

		It("asks again once the refusal has gone stale", func() {
			resolver.setErr(errors.New("pull request 12 is closed"))
			_, err := mgr.AutoStart(ctx, host)
			Expect(err).To(HaveOccurred())

			clock.advance(previewDenialTTL + time.Second)
			resolver.setErr(nil)

			p, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.PR).To(Equal("12"))
			Expect(resolver.count()).To(Equal(2))
		})

		It("stops resolving new hostnames once the window's budget is spent", func() {
			// A pull request number is a small integer, so walking them is easy;
			// what this bounds is how fast that walk can reach the forge.
			resolver.setErr(preview.ErrNoRoute)
			for i := range previewAutoStartBurst {
				_, err := mgr.AutoStart(ctx, hostFor(i))
				Expect(err).To(MatchError(preview.ErrNoRoute))
			}
			Expect(resolver.count()).To(Equal(previewAutoStartBurst))

			_, err := mgr.AutoStart(ctx, hostFor(previewAutoStartBurst))
			Expect(err).To(MatchError(ErrAutoStartUnavailable))
			Expect(resolver.count()).To(Equal(previewAutoStartBurst))
		})

		It("starts a fresh budget in the next window", func() {
			resolver.setErr(preview.ErrNoRoute)
			for i := range previewAutoStartBurst {
				_, err := mgr.AutoStart(ctx, hostFor(i))
				Expect(err).To(MatchError(preview.ErrNoRoute))
			}
			clock.advance(previewAutoStartWindow + time.Second)

			_, err := mgr.AutoStart(ctx, hostFor(previewAutoStartBurst))
			Expect(err).To(MatchError(preview.ErrNoRoute))
			Expect(resolver.count()).To(Equal(previewAutoStartBurst + 1))
		})
	})

	Describe("forgetting a pull request that closed", func() {
		var states map[string]string

		// stateOf answers for whichever pull request is asked about, and records
		// nothing else: the sweep's decisions are all it is being asked to show.
		BeforeEach(func() {
			states = map[string]string{}
			mgr.prState = func(_ context.Context, _, pr string) (string, error) {
				state, ok := states[pr]
				if !ok {
					return "", errors.New("no such pull request")
				}
				return state, nil
			}
		})

		autoStarted := func() *preview.Preview {
			GinkgoHelper()
			p, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			return p
		}

		It("removes the preview of a merged pull request, freeing its hostname", func() {
			p := autoStarted()
			states["12"] = "merged"

			mgr.ReapClosedPullRequests(ctx)

			_, err := store.Get(ctx, p.ID)
			Expect(err).To(MatchError(preview.ErrNotFound))
		})

		It("keeps the preview of an open pull request", func() {
			p := autoStarted()
			states["12"] = "open"

			mgr.ReapClosedPullRequests(ctx)

			_, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
		})

		It("keeps a preview whose pull request could not be read", func() {
			// A forge that cannot be reached is not evidence that anything
			// closed, and removing on that reading takes down something in use.
			p := autoStarted()

			mgr.ReapClosedPullRequests(ctx)

			_, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
		})

		It("never removes a preview an operator started by hand", func() {
			p, err := mgr.Register(ctx, "acme", "main", previewOrigin{PR: "12"})
			Expect(err).NotTo(HaveOccurred())
			states["12"] = "closed"

			mgr.ReapClosedPullRequests(ctx)

			_, err = store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
		})

		It("never removes a preview an operator started by hand and a request then found", func() {
			// The pull request's hostname resolves to the same branch, so the
			// request joins the operator's preview. Joining must not hand that
			// preview's lifetime to whoever merges the pull request.
			p, err := mgr.Register(ctx, "acme", "feature/login", previewOrigin{})
			Expect(err).NotTo(HaveOccurred())
			joined, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			Expect(joined.ID).To(Equal(p.ID))

			states["12"] = "merged"
			mgr.ReapClosedPullRequests(ctx)

			_, err = store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
		})

		It("lets the hostname start a preview again after the record is gone", func() {
			autoStarted()
			states["12"] = "closed"
			mgr.ReapClosedPullRequests(ctx)

			// The refusal cache must not answer for a name that is free again.
			p, err := mgr.AutoStart(ctx, host)
			Expect(err).NotTo(HaveOccurred())
			Expect(p.AutoStartHost).To(Equal(host))
		})
	})
})

// hostFor builds a distinct auto-start hostname per index.
func hostFor(i int) string {
	return "pr-" + strconv.Itoa(i) + ".preview.example.com"
}

// previewProjectStore is the smallest project store the route table needs.
type previewProjectStore struct {
	projects map[string]*projcfg.Project
}

func (s *previewProjectStore) Get(_ context.Context, name string) (*projcfg.Project, error) {
	p, ok := s.projects[name]
	if !ok {
		return nil, errors.New("no such project")
	}
	return p, nil
}

func (s *previewProjectStore) List(_ context.Context) ([]*projcfg.Project, error) {
	out := make([]*projcfg.Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	return out, nil
}

func (s *previewProjectStore) Put(_ context.Context, p *projcfg.Project) error {
	s.projects[p.Name] = p
	return nil
}

func (s *previewProjectStore) Delete(_ context.Context, name string) error {
	delete(s.projects, name)
	return nil
}

var _ = Describe("Preview auto-start route table", func() {
	var (
		ctx      context.Context
		projects map[string]*projcfg.Project
		svc      *Service
	)

	// project builds a configured project with the given preview block.
	newProject := func(name string, prev projcfg.Preview) *projcfg.Project {
		return &projcfg.Project{Name: name, RepoURL: "org/" + name, Preview: prev}
	}

	BeforeEach(func() {
		ctx = context.Background()
		projects = map[string]*projcfg.Project{}
		svc = NewServiceWithOpts(ServiceOpts{
			ProjectStore:  &previewProjectStore{projects: projects},
			PreviewStore:  preview.NewMemStore(),
			PreviewPolicy: PreviewPolicy{Domain: "preview.example.com"},
		})
	})

	It("claims nothing when no project has auto-start configured", func() {
		projects["acme"] = newProject("acme", projcfg.Preview{})

		_, err := svc.previewRouter(ctx)
		Expect(err).To(MatchError(preview.ErrNoRoute))
	})

	It("forms names under the operator's domain", func() {
		projects["acme"] = newProject("acme", projcfg.Preview{AutoStart: []string{"pr-{pr}.{domain}"}})

		router, err := svc.previewRouter(ctx)
		Expect(err).NotTo(HaveOccurred())

		match, err := router.Match("pr-9.preview.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(match.Project).To(Equal("acme"))
		Expect(match.PR).To(Equal("9"))
	})

	It("forms names under a project's own domain when it has one", func() {
		projects["acme"] = newProject("acme", projcfg.Preview{
			Domain:    "acme.example.com",
			AutoStart: []string{"pr-{pr}.{domain}"},
		})

		router, err := svc.previewRouter(ctx)
		Expect(err).NotTo(HaveOccurred())

		match, err := router.Match("pr-9.acme.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(match.Project).To(Equal("acme"))

		_, err = router.Match("pr-9.preview.example.com")
		Expect(err).To(MatchError(preview.ErrNoRoute))
	})

	It("claims nothing for a project whose previews are turned off", func() {
		off := false
		projects["acme"] = newProject("acme", projcfg.Preview{
			Enabled:   &off,
			AutoStart: []string{"pr-{pr}.{domain}"},
		})

		_, err := svc.previewRouter(ctx)
		Expect(err).To(MatchError(preview.ErrNoRoute))
	})

	It("keeps a project's usable patterns when one of them is not", func() {
		// A typo in one pattern must not take a working project's routes with
		// it, nor another project's.
		projects["acme"] = newProject("acme", projcfg.Preview{
			AutoStart: []string{"pr-{pr}.example.org", "pr-{pr}.{domain}"},
		})

		router, err := svc.previewRouter(ctx)
		Expect(err).NotTo(HaveOccurred())

		match, err := router.Match("pr-4.preview.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(match.Project).To(Equal("acme"))
	})
})

var _ = Describe("Preview boot hostname check", func() {
	It("passes when a site answers on the name that asked for the preview", func() {
		p := &preview.Preview{Ref: "feature/login", AutoStartHost: "pr-12.preview.example.com"}
		sites := []preview.Site{{Name: "web", Host: "pr-12.preview.example.com", Port: 3000}}
		Expect(checkAutoStartHost(p, sites)).To(Succeed())
	})

	It("fails a boot whose sites answer on some other name", func() {
		// The VM would come up perfectly and the browser waiting on the holding
		// page would reload into a 404 with nothing to explain it.
		p := &preview.Preview{Ref: "feature/login", AutoStartHost: "pr-12.preview.example.com"}
		sites := []preview.Site{{Name: "web", Host: "feature-login.preview.example.com", Port: 3000}}

		err := checkAutoStartHost(p, sites)
		Expect(err).To(MatchError(ContainSubstring("pr-12.preview.example.com")))
		Expect(err).To(MatchError(ContainSubstring("feature-login.preview.example.com")))
	})

	It("says nothing about a preview that was registered explicitly", func() {
		p := &preview.Preview{Ref: "main"}
		sites := []preview.Site{{Name: "web", Host: "main.preview.example.com", Port: 3000}}
		Expect(checkAutoStartHost(p, sites)).To(Succeed())
	})
})
