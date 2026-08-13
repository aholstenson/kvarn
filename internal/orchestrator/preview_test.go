package orchestrator

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	"github.com/aholstenson/kvarn/internal/preview"
)

// The manager specs live in-package: the manager is deliberately not exported
// (a preview is reached through the service's RPCs, not by constructing one),
// and its capacity, eviction and reaping logic is exactly the part worth
// testing without a VM anywhere near it.

// fakePreviewClock is a manually advanced clock, so idle and lifetime reaping
// can be driven without sleeping.
type fakePreviewClock struct {
	mu  sync.Mutex
	now time.Time
}

func newPreviewClock() *fakePreviewClock {
	return &fakePreviewClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
}

func (c *fakePreviewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakePreviewClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakePreviewSandbox records that it was closed, standing in for a VM.
type fakePreviewSandbox struct {
	mu     sync.Mutex
	closed bool
}

func (f *fakePreviewSandbox) DialGuest(context.Context, uint16) (net.Conn, error) {
	return nil, errors.New("not dialable in this spec")
}

func (f *fakePreviewSandbox) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakePreviewSandbox) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeBooter records boots and hands back whatever the spec configured.
type fakeBooter struct {
	mu sync.Mutex
	// err, when set, is returned instead of booting.
	err error
	// hostFor maps a preview ID to the hostname its single app answers on.
	hostFor map[string]string
	// boots counts calls per preview ID.
	boots map[string]int
	// sandboxes records the sandbox handed back per preview ID.
	sandboxes map[string]*fakePreviewSandbox
	// block, when non-nil, holds each boot until it is closed.
	block chan struct{}
}

func newFakeBooter() *fakeBooter {
	return &fakeBooter{
		hostFor:   map[string]string{},
		boots:     map[string]int{},
		sandboxes: map[string]*fakePreviewSandbox{},
	}
}

func (b *fakeBooter) boot(_ context.Context, p *preview.Preview, logs *preview.LogBuffer) (*previewBoot, error) {
	b.mu.Lock()
	block := b.block
	err := b.err
	b.boots[p.ID]++
	host := b.hostFor[p.ID]
	b.mu.Unlock()

	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	if host == "" {
		host = p.Ref + ".preview.example.com"
	}

	sb := &fakePreviewSandbox{}
	b.mu.Lock()
	b.sandboxes[p.ID] = sb
	b.mu.Unlock()

	logs.Append("==> booted " + p.ID + "\n")
	return &previewBoot{
		Sandbox:   sb,
		Apps:      []preview.App{{Name: "web", Host: host, Port: 3000}},
		SessionID: "sess-" + p.ID,
		Lease:     nil,
	}, nil
}

func (b *fakeBooter) bootCount(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.boots[id]
}

func (b *fakeBooter) sandbox(id string) *fakePreviewSandbox {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sandboxes[id]
}

func (b *fakeBooter) setErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

var _ = Describe("Preview manager", func() {
	var (
		ctx    context.Context
		store  preview.Store
		clock  *fakePreviewClock
		booter *fakeBooter
		mgr    *previewManager
	)

	// build makes a manager with the given policy, wired to the shared fakes.
	build := func(policy PreviewPolicy) *previewManager {
		if policy.Domain == "" {
			policy.Domain = "preview.example.com"
		}
		m := newPreviewManager(store, policy, booter.boot)
		m.now = clock.Now
		return m
	}

	// bootAndWait starts a preview and waits for it to reach running.
	bootAndWait := func(m *previewManager, project, ref string) *preview.Preview {
		GinkgoHelper()
		p, err := m.Register(ctx, project, ref)
		Expect(err).NotTo(HaveOccurred())
		_, err = m.Ensure(ctx, p.ID)
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool { return m.IsLive(p.ID) }).Should(BeTrue())
		Eventually(func() preview.State {
			got, err := store.Get(ctx, p.ID)
			if err != nil {
				return ""
			}
			return got.State
		}).Should(Equal(preview.StateRunning))
		got, err := store.Get(ctx, p.ID)
		Expect(err).NotTo(HaveOccurred())
		return got
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = preview.NewMemStore()
		clock = newPreviewClock()
		booter = newFakeBooter()
		mgr = build(PreviewPolicy{})
		DeferCleanup(func() { Expect(store.Close()).To(Succeed()) })
	})

	Describe("registration", func() {
		It("creates a stopped record without booting anything", func() {
			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			Expect(p.ID).To(Equal(preview.ID("proj", "main")))
			Expect(p.State).To(Equal(preview.StateStopped))
			Expect(booter.bootCount(p.ID)).To(Equal(0))
		})

		It("is idempotent for a ref that already has a preview", func() {
			first, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			second, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			Expect(second.ID).To(Equal(first.ID))
			Expect(second.CreatedAt).To(BeTemporally("==", first.CreatedAt))
		})

		It("reports previews as disabled without a domain", func() {
			disabled := newPreviewManager(store, PreviewPolicy{}, booter.boot)
			_, err := disabled.Register(ctx, "proj", "main")
			Expect(err).To(MatchError(ErrPreviewsDisabled))
		})

		It("reports previews as disabled without a store", func() {
			disabled := newPreviewManager(nil, PreviewPolicy{Domain: "preview.example.com"}, booter.boot)
			_, err := disabled.Register(ctx, "proj", "main")
			Expect(err).To(MatchError(ErrPreviewsDisabled))
		})
	})

	Describe("booting", func() {
		It("boots on Ensure and records the resolved apps", func() {
			got := bootAndWait(mgr, "proj", "main")
			Expect(got.State).To(Equal(preview.StateRunning))
			Expect(got.Apps).To(HaveLen(1))
			Expect(got.Apps[0].Host).To(Equal("main.preview.example.com"))
			Expect(got.SessionID).To(Equal("sess-proj/main"))
			Expect(got.StartedAt).To(BeTemporally("==", clock.Now()))
		})

		It("makes the preview routable by hostname once it is running", func() {
			bootAndWait(mgr, "proj", "main")
			found, err := mgr.FindByHost(ctx, "main.preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(found.ID).To(Equal("proj/main"))
		})

		It("single-flights a burst of requests into one boot", func() {
			booter.block = make(chan struct{})

			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			for range 10 {
				_, err := mgr.Ensure(ctx, p.ID)
				Expect(err).NotTo(HaveOccurred())
			}
			Eventually(func() int { return booter.bootCount(p.ID) }).Should(Equal(1))

			close(booter.block)
			Eventually(func() bool { return mgr.IsLive(p.ID) }).Should(BeTrue())
			Expect(booter.bootCount(p.ID)).To(Equal(1))
		})

		It("reports a booting preview without waiting for the boot", func() {
			booter.block = make(chan struct{})
			defer close(booter.block)

			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() preview.State {
				got, err := store.Get(ctx, p.ID)
				if err != nil {
					return ""
				}
				return got.State
			}).Should(Equal(preview.StateBooting))
			Expect(mgr.IsLive(p.ID)).To(BeFalse())
		})

		It("records a failed boot with its reason", func() {
			booter.setErr(errors.New("setup step \"build\" failed"))

			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() preview.State {
				got, err := store.Get(ctx, p.ID)
				if err != nil {
					return ""
				}
				return got.State
			}).Should(Equal(preview.StateFailed))

			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Error).To(ContainSubstring("build"))
			Expect(mgr.IsLive(p.ID)).To(BeFalse())
		})

		It("retries a failed preview on the next request", func() {
			booter.setErr(errors.New("transient"))
			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() int { return booter.bootCount(p.ID) }).Should(Equal(1))

			booter.setErr(nil)
			Eventually(func() error {
				_, err := mgr.Ensure(ctx, p.ID)
				return err
			}).Should(Succeed())
			Eventually(func() bool { return mgr.IsLive(p.ID) }).Should(BeTrue())
		})

		It("keeps a preview's log tail available", func() {
			p := bootAndWait(mgr, "proj", "main")
			Expect(mgr.Logs(p.ID, 0)).To(ContainSubstring("booted proj/main"))
		})
	})

	Describe("stopping", func() {
		It("tears the VM down and leaves the record routable", func() {
			p := bootAndWait(mgr, "proj", "main")
			sb := booter.sandbox(p.ID)

			Expect(mgr.Stop(ctx, p.ID, "spec")).To(Succeed())
			Expect(sb.isClosed()).To(BeTrue())
			Expect(mgr.IsLive(p.ID)).To(BeFalse())

			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopped))
			Expect(got.StartedAt.IsZero()).To(BeTrue())
			Expect(got.ExpiresAt.IsZero()).To(BeTrue())

			// The hostname still routes, which is what lets the next request
			// boot it again rather than 404.
			found, err := mgr.FindByHost(ctx, "main.preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(found.ID).To(Equal(p.ID))
		})

		It("boots again on the next request", func() {
			p := bootAndWait(mgr, "proj", "main")
			Expect(mgr.Stop(ctx, p.ID, "spec")).To(Succeed())

			_, err := mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return mgr.IsLive(p.ID) }).Should(BeTrue())
			Expect(booter.bootCount(p.ID)).To(Equal(2))
		})

		It("is a no-op for a preview that is already stopped", func() {
			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			Expect(mgr.Stop(ctx, p.ID, "spec")).To(Succeed())
		})

		It("removes a preview and frees its hostnames", func() {
			p := bootAndWait(mgr, "proj", "main")
			sb := booter.sandbox(p.ID)

			Expect(mgr.Remove(ctx, p.ID)).To(Succeed())
			Expect(sb.isClosed()).To(BeTrue())
			_, err := store.Get(ctx, p.ID)
			Expect(err).To(MatchError(preview.ErrNotFound))
			_, err = mgr.FindByHost(ctx, "main.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNotFound))
		})
	})

	Describe("eviction", func() {
		It("evicts the least-recently-requested preview when at max_concurrent", func() {
			mgr = build(PreviewPolicy{MaxConcurrent: 2})

			first := bootAndWait(mgr, "proj", "a")
			clock.advance(time.Minute)
			second := bootAndWait(mgr, "proj", "b")

			// Touch the older one so it is the more recently wanted of the two.
			clock.advance(time.Minute)
			mgr.Touch(ctx, first.ID)

			clock.advance(time.Minute)
			third := bootAndWait(mgr, "proj", "c")

			Expect(mgr.IsLive(third.ID)).To(BeTrue())
			Expect(mgr.IsLive(second.ID)).To(BeFalse(), "the least-recently-requested preview should have been evicted")
			Expect(mgr.IsLive(first.ID)).To(BeTrue())

			got, err := store.Get(ctx, second.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopped))
		})

		It("evicts and retries when the scheduler has no room", func() {
			mgr = build(PreviewPolicy{})
			resident := bootAndWait(mgr, "proj", "a")

			// The next boot is refused for capacity once, then succeeds.
			attempts := 0
			mgr.boot = func(ctx context.Context, p *preview.Preview, logs *preview.LogBuffer) (*previewBoot, error) {
				attempts++
				if attempts == 1 {
					return nil, scheduler.ErrWouldBlock
				}
				return booter.boot(ctx, p, logs)
			}

			p, err := mgr.Register(ctx, "proj", "b")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool { return mgr.IsLive(p.ID) }).Should(BeTrue())
			Expect(mgr.IsLive(resident.ID)).To(BeFalse())
			Expect(attempts).To(Equal(2))
		})

		It("reports at-capacity when there is nothing idle to evict", func() {
			mgr = build(PreviewPolicy{})
			mgr.boot = func(context.Context, *preview.Preview, *preview.LogBuffer) (*previewBoot, error) {
				return nil, scheduler.ErrWouldBlock
			}

			p, err := mgr.Register(ctx, "proj", "only")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() string {
				got, err := store.Get(ctx, p.ID)
				if err != nil {
					return ""
				}
				return got.Error
			}).Should(ContainSubstring(ErrAtCapacity.Error()))
		})

		It("never picks the preview being booted as the eviction victim", func() {
			mgr = build(PreviewPolicy{MaxConcurrent: 1})
			resident := bootAndWait(mgr, "proj", "a")

			p, err := mgr.Register(ctx, "proj", "b")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool { return mgr.IsLive(p.ID) }).Should(BeTrue())
			Expect(mgr.IsLive(resident.ID)).To(BeFalse())
		})
	})

	Describe("reaping", func() {
		It("stops a preview that has gone idle", func() {
			mgr = build(PreviewPolicy{IdleTimeout: 30 * time.Minute})
			p := bootAndWait(mgr, "proj", "main")

			clock.advance(29 * time.Minute)
			mgr.Reap(ctx)
			Expect(mgr.IsLive(p.ID)).To(BeTrue())

			clock.advance(2 * time.Minute)
			mgr.Reap(ctx)
			Expect(mgr.IsLive(p.ID)).To(BeFalse())

			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopped))
		})

		It("keeps a preview alive while requests keep arriving", func() {
			mgr = build(PreviewPolicy{IdleTimeout: 30 * time.Minute})
			p := bootAndWait(mgr, "proj", "main")

			for range 5 {
				clock.advance(20 * time.Minute)
				mgr.Touch(ctx, p.ID)
				mgr.Reap(ctx)
			}
			Expect(mgr.IsLive(p.ID)).To(BeTrue())
		})

		It("stops a preview that has outlived max_lifetime whatever its traffic", func() {
			mgr = build(PreviewPolicy{IdleTimeout: time.Hour, MaxLifetime: 4 * time.Hour})
			p := bootAndWait(mgr, "proj", "main")

			for range 8 {
				clock.advance(30 * time.Minute)
				mgr.Touch(ctx, p.ID)
				mgr.Reap(ctx)
			}
			Expect(mgr.IsLive(p.ID)).To(BeFalse())
		})

		It("never reaps when neither cap is configured", func() {
			mgr = build(PreviewPolicy{})
			p := bootAndWait(mgr, "proj", "main")

			clock.advance(30 * 24 * time.Hour)
			mgr.Reap(ctx)
			Expect(mgr.IsLive(p.ID)).To(BeTrue())
		})
	})

	Describe("restart reconciliation", func() {
		It("resets rows a dead process left non-stopped", func() {
			// A previous process's state: rows say running, but this manager
			// holds no VMs, because they died with that process.
			for _, ref := range []string{"a", "b"} {
				id := preview.ID("proj", ref)
				Expect(store.Put(ctx, &preview.Preview{
					ID:        id,
					Project:   "proj",
					Ref:       ref,
					State:     preview.StateRunning,
					Apps:      []preview.App{{Name: "web", Host: ref + ".preview.example.com", Port: 3000}},
					SessionID: "old-session",
					StartedAt: clock.Now(),
					CreatedAt: clock.Now(),
					UpdatedAt: clock.Now(),
				})).To(Succeed())
			}

			fresh := build(PreviewPolicy{})
			Expect(fresh.Reconcile(ctx)).To(Succeed())

			for _, ref := range []string{"a", "b"} {
				got, err := store.Get(ctx, preview.ID("proj", ref))
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(preview.StateStopped))
				Expect(got.SessionID).To(BeEmpty())
				Expect(got.StartedAt.IsZero()).To(BeTrue())
				// The hostname survives, so the next request boots it again.
				Expect(got.Apps).To(HaveLen(1))
			}
		})

		It("boots a reconciled preview again on the next request", func() {
			id := preview.ID("proj", "main")
			Expect(store.Put(ctx, &preview.Preview{
				ID: id, Project: "proj", Ref: "main", State: preview.StateBooting,
				CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
			})).To(Succeed())

			fresh := build(PreviewPolicy{})
			Expect(fresh.Reconcile(ctx)).To(Succeed())

			_, err := fresh.Ensure(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return fresh.IsLive(id) }).Should(BeTrue())
		})
	})

	Describe("draining", func() {
		It("stops every running preview and refuses new boots", func() {
			a := bootAndWait(mgr, "proj", "a")
			b := bootAndWait(mgr, "proj", "b")

			mgr.SetDraining(ctx, true)

			Expect(mgr.IsLive(a.ID)).To(BeFalse())
			Expect(mgr.IsLive(b.ID)).To(BeFalse())
			Expect(booter.sandbox(a.ID).isClosed()).To(BeTrue())
			Expect(booter.sandbox(b.ID).isClosed()).To(BeTrue())

			_, err := mgr.Ensure(ctx, a.ID)
			Expect(err).To(MatchError(ErrPreviewDraining))
			Consistently(func() int { return booter.bootCount(a.ID) }, 100*time.Millisecond).Should(Equal(1))
		})

		It("boots again once the drain is lifted", func() {
			p := bootAndWait(mgr, "proj", "main")
			mgr.SetDraining(ctx, true)
			mgr.SetDraining(ctx, false)

			_, err := mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return mgr.IsLive(p.ID) }).Should(BeTrue())
		})

		It("tears down a boot that finished after the drain started", func() {
			booter.block = make(chan struct{})

			p, err := mgr.Register(ctx, "proj", "main")
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() int { return booter.bootCount(p.ID) }).Should(Equal(1))

			mgr.SetDraining(ctx, true)
			close(booter.block)

			Eventually(func() bool {
				sb := booter.sandbox(p.ID)
				return sb != nil && sb.isClosed()
			}).Should(BeTrue())
			Expect(mgr.IsLive(p.ID)).To(BeFalse())
		})
	})

	Describe("shutdown", func() {
		It("stops every running preview", func() {
			a := bootAndWait(mgr, "proj", "a")
			b := bootAndWait(mgr, "proj", "b")

			mgr.Shutdown(ctx)

			Expect(booter.sandbox(a.ID).isClosed()).To(BeTrue())
			Expect(booter.sandbox(b.ID).isClosed()).To(BeTrue())
			Expect(mgr.IsLive(a.ID)).To(BeFalse())
			Expect(mgr.IsLive(b.ID)).To(BeFalse())
		})
	})
})
