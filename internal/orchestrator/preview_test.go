package orchestrator

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/orchestrator/scheduler"
	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	projconfig "github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
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

// fakePreviewSandbox records that it was closed, standing in for a VM. Its
// guest answers the scripts a state capture runs, so the specs can tell a
// preview that saved itself from one that did not.
type fakePreviewSandbox struct {
	guest *guestRecorder

	mu     sync.Mutex
	closed bool
}

func newFakePreviewSandbox() *fakePreviewSandbox {
	return &fakePreviewSandbox{guest: newGuestRecorder()}
}

func (f *fakePreviewSandbox) DialGuest(context.Context, uint16) (net.Conn, error) {
	return nil, errors.New("not dialable in this spec")
}

func (f *fakePreviewSandbox) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func (f *fakePreviewSandbox) BareRunner() sandbox.RunnerProxy  { return f.guest }
func (f *fakePreviewSandbox) GetRunner() sandbox.RunnerProxy   { return f.guest }
func (f *fakePreviewSandbox) GetShellSessionID() string        { return "shell" }
func (f *fakePreviewSandbox) Processes() sandbox.ProcessRunner { return f.guest }

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
	// cfg is the kvarn.yml every booted preview is treated as having come up
	// on, which is what says whether it keeps state and which servers to stop.
	cfg *projconfig.Config
	// hasState makes the booted guest answer the state probe with "yes", for a
	// repository that declares nothing but writes into the state directory.
	hasState bool
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

	sb := newFakePreviewSandbox()
	b.mu.Lock()
	sb.guest.hasState = b.hasState
	cfg := b.cfg
	b.sandboxes[p.ID] = sb
	b.mu.Unlock()

	if cfg == nil {
		cfg = &projconfig.Config{}
	}

	logs.Append("==> booted " + p.ID + "\n")
	return &previewBoot{
		Sandbox:    sb,
		Sites:      []preview.Site{{Name: "web", Host: host, Port: 3000}},
		SessionID:  "sess-" + p.ID,
		Lease:      nil,
		Config:     cfg,
		SnapshotID: snapshot.ID{ProjectID: "proj", RefLabel: projconfig.RefLabel(p.Ref)},
		Commit:     "abc1234",
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
		p, err := m.Register(ctx, project, ref, previewOrigin{})
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
			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.ID).To(Equal(preview.ID("proj", "main")))
			Expect(p.State).To(Equal(preview.StateStopped))
			Expect(booter.bootCount(p.ID)).To(Equal(0))
		})

		It("is idempotent for a ref that already has a preview", func() {
			first, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
			Expect(err).NotTo(HaveOccurred())
			second, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
			Expect(err).NotTo(HaveOccurred())
			Expect(second.ID).To(Equal(first.ID))
			Expect(second.CreatedAt).To(BeTemporally("==", first.CreatedAt))
		})

		It("reports previews as disabled without a domain", func() {
			disabled := newPreviewManager(store, PreviewPolicy{}, booter.boot)
			_, err := disabled.Register(ctx, "proj", "main", previewOrigin{})
			Expect(err).To(MatchError(ErrPreviewsDisabled))
		})

		It("reports previews as disabled without a store", func() {
			disabled := newPreviewManager(nil, PreviewPolicy{Domain: "preview.example.com"}, booter.boot)
			_, err := disabled.Register(ctx, "proj", "main", previewOrigin{})
			Expect(err).To(MatchError(ErrPreviewsDisabled))
		})
	})

	Describe("booting", func() {
		It("boots on Ensure and records the resolved sites", func() {
			got := bootAndWait(mgr, "proj", "main")
			Expect(got.State).To(Equal(preview.StateRunning))
			Expect(got.Sites).To(HaveLen(1))
			Expect(got.Sites[0].Host).To(Equal("main.preview.example.com"))
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

			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
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

			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
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

			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
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

		// failOnce boots a preview whose boot fails, and returns its record.
		failOnce := func() *preview.Preview {
			GinkgoHelper()
			booter.setErr(errors.New("transient"))
			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
			Expect(err).NotTo(HaveOccurred())
			_, err = mgr.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() int { return booter.bootCount(p.ID) }).Should(Equal(1))
			Eventually(func() preview.State {
				got, err := store.Get(ctx, p.ID)
				if err != nil {
					return ""
				}
				return got.State
			}).Should(Equal(preview.StateFailed))
			booter.setErr(nil)
			return p
		}

		It("does not repeat a boot that just failed", func() {
			// Ingress reaches Ensure on every request for the hostname, and a
			// preview that cannot come up would otherwise cost a clone and a VM
			// per page load.
			p := failOnce()

			for range 5 {
				_, err := mgr.Ensure(ctx, p.ID)
				Expect(err).NotTo(HaveOccurred())
			}
			Consistently(func() int { return booter.bootCount(p.ID) }).Should(Equal(1))
		})

		It("retries a failed preview once the backoff has passed", func() {
			p := failOnce()

			clock.advance(previewBootRetryDelay)
			// Asked repeatedly because the failed boot releases its single-flight
			// entry just after it records the failure, and a request landing in
			// that gap joins the boot that is already ending.
			Eventually(func() bool {
				_, err := mgr.Ensure(ctx, p.ID)
				Expect(err).NotTo(HaveOccurred())
				return mgr.IsLive(p.ID)
			}).Should(BeTrue())
		})

		It("retries a failed preview immediately when it is started explicitly", func() {
			p := failOnce()

			Eventually(func() bool {
				_, err := mgr.EnsureNow(ctx, p.ID)
				Expect(err).NotTo(HaveOccurred())
				return mgr.IsLive(p.ID)
			}).Should(BeTrue())
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
			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
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

	Describe("saving state", func() {
		var snapshots *snapshot.FileStore

		// stateful wires the manager to a state store and makes every booted
		// preview one that declares state, which is the case worth testing; the
		// stateless one is every spec above.
		stateful := func(policy PreviewPolicy) *previewManager {
			snapshots = snapshot.NewFileStore(GinkgoT().TempDir())
			booter.cfg = &projconfig.Config{Preview: projconfig.Preview{
				Sites: map[string]projconfig.PreviewSite{"web": {Port: 3000}},
				Serve: []projconfig.PreviewProcess{{Name: "web", Run: "npm start"}},
				State: projconfig.PreviewState{Paths: []string{"/home/kvarn/pgdata"}},
			}}
			m := build(policy)
			m.snapshots = snapshots
			m.snapshotIDs = func(_ context.Context, p *preview.Preview) (snapshot.ID, error) {
				return snapshot.ID{ProjectID: "proj", RefLabel: projconfig.RefLabel(p.Ref)}, nil
			}
			return m
		}

		// savedState is what the store holds for a preview, or "" for nothing.
		savedState := func(ref string) string {
			GinkgoHelper()
			r, _, err := snapshots.Open(snapshot.ID{ProjectID: "proj", RefLabel: projconfig.RefLabel(ref)})
			if errors.Is(err, snapshot.ErrNoSnapshot) {
				return ""
			}
			Expect(err).NotTo(HaveOccurred())
			defer r.Close()
			body, err := io.ReadAll(r)
			Expect(err).NotTo(HaveOccurred())
			return string(body)
		}

		DescribeTable("captures on every graceful stop",
			func(stop func(m *previewManager, id string)) {
				m := stateful(PreviewPolicy{IdleTimeout: time.Hour})
				p := bootAndWait(m, "proj", "main")

				stop(m, p.ID)

				Eventually(func() string { return savedState("main") }).Should(Equal("tar-bytes"))
				got, err := store.Get(ctx, p.ID)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(preview.StateStopped))
				Expect(got.StateBytes).To(Equal(int64(len("tar-bytes"))))
				Expect(got.StateSavedAt.IsZero()).To(BeFalse())
				Expect(got.StateError).To(BeEmpty())
			},
			Entry("an explicit stop", func(m *previewManager, id string) {
				Expect(m.Stop(ctx, id, "spec")).To(Succeed())
			}),
			Entry("idle reaping", func(m *previewManager, id string) {
				clock.advance(2 * time.Hour)
				m.Reap(ctx)
			}),
			Entry("draining", func(m *previewManager, id string) {
				m.SetDraining(ctx, true)
			}),
			Entry("shutdown", func(m *previewManager, id string) {
				m.Shutdown(ctx)
			}),
		)

		It("stops the preview's servers before archiving what they were writing", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")
			guest := booter.sandbox(p.ID).guest

			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())

			Expect(guest.stoppedProcesses()).To(Equal([]string{p.ID + "/serve-0"}))
			Expect(guest.events()).To(ContainElements("stop:"+p.ID+"/serve-0", "guest:tar"))
			Expect(indexOf(guest.events(), "stop:"+p.ID+"/serve-0")).
				To(BeNumerically("<", indexOf(guest.events(), "guest:tar")))
		})

		It("captures a preview that declares nothing but wrote into the state directory", func() {
			booter.hasState = true
			m := stateful(PreviewPolicy{})
			booter.cfg = &projconfig.Config{Preview: projconfig.Preview{
				Sites: map[string]projconfig.PreviewSite{"web": {Port: 3000}},
			}}
			p := bootAndWait(m, "proj", "main")

			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())
			Expect(savedState("main")).To(Equal("tar-bytes"))
		})

		It("leaves a preview that holds nothing alone", func() {
			m := stateful(PreviewPolicy{})
			booter.cfg = &projconfig.Config{Preview: projconfig.Preview{
				Sites: map[string]projconfig.PreviewSite{"web": {Port: 3000}},
			}}
			p := bootAndWait(m, "proj", "main")
			guest := booter.sandbox(p.ID).guest

			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())

			Expect(savedState("main")).To(BeEmpty())
			Expect(guest.ranTar()).To(BeFalse())
			// It never enters "stopping" either: a stateless preview tears down
			// exactly as fast as it did before any of this existed.
			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopped))
		})

		It("never captures a preview of a fork's pull request", func() {
			m := stateful(PreviewPolicy{})
			p, err := m.Register(ctx, "proj", "main", previewOrigin{PR: "7", AutoStartHost: "pr-7.preview.example.com", Fork: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(p.Fork).To(BeTrue())
			_, err = m.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return m.IsLive(p.ID) }).Should(BeTrue())

			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())
			Expect(savedState("main")).To(BeEmpty())
		})

		It("never captures on the way to removing a preview, and drops what it had", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")
			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())
			Expect(savedState("main")).To(Equal("tar-bytes"))

			_, err := m.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return m.IsLive(p.ID) }).Should(BeTrue())

			Expect(m.Remove(ctx, p.ID)).To(Succeed())
			Expect(savedState("main")).To(BeEmpty())
		})

		It("skips the capture when the caller asked for the contents to go", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")

			Expect(m.StopWithoutState(ctx, p.ID, "spec")).To(Succeed())
			Expect(savedState("main")).To(BeEmpty())
			Expect(booter.sandbox(p.ID).isClosed()).To(BeTrue())
		})

		It("still stops the preview when the capture fails, and says why", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")
			booter.sandbox(p.ID).guest.tarFails = true

			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())

			Expect(booter.sandbox(p.ID).isClosed()).To(BeTrue())
			Expect(m.IsLive(p.ID)).To(BeFalse())
			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopped))
			Expect(got.StateError).To(ContainSubstring("archive preview state"))
		})

		It("refuses an archive over the operator's ceiling", func() {
			m := stateful(PreviewPolicy{MaxStateBytes: 2})
			p := bootAndWait(m, "proj", "main")

			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())

			Expect(savedState("main")).To(BeEmpty())
			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.StateError).To(ContainSubstring("maximum size"))
		})

		It("does not boot a second VM for a request that arrives mid-capture", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")

			// The row a capture leaves behind while it works is what a request
			// arriving in that window sees; booting on top of it would restore the
			// archive the capture is about to replace.
			p.State = preview.StateStopping
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := m.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopping))
			Expect(booter.bootCount(p.ID)).To(Equal(1))
		})

		It("leaves a capture in progress alone when a second stop arrives", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")
			Expect(m.stopInstance(p.ID)).To(BeTrue())

			// Standing in for the winner: the row says a capture is under way and
			// the instance is already claimed.
			p.State = preview.StateStopping
			Expect(store.Put(ctx, p)).To(Succeed())

			Expect(m.Stop(ctx, p.ID, "the loser")).To(Succeed())
			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateStopping))
		})

		It("restores the prune horizon rather than sweeping a live preview", func() {
			m := stateful(PreviewPolicy{StateRetention: time.Nanosecond})
			p := bootAndWait(m, "proj", "main")
			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())
			Expect(savedState("main")).To(Equal("tar-bytes"))

			// Booted again, so its archive is about to be rewritten by the stop
			// that takes it down; sweeping it now would turn that stop into a
			// discard.
			_, err := m.Ensure(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool { return m.IsLive(p.ID) }).Should(BeTrue())

			m.PruneState(ctx)
			Expect(savedState("main")).To(Equal("tar-bytes"))

			Expect(m.StopWithoutState(ctx, p.ID, "spec")).To(Succeed())
			m.PruneState(ctx)
			Expect(savedState("main")).To(BeEmpty())
		})

		It("drops a preview's stored state on request", func() {
			m := stateful(PreviewPolicy{})
			p := bootAndWait(m, "proj", "main")
			Expect(m.Stop(ctx, p.ID, "spec")).To(Succeed())
			Expect(savedState("main")).To(Equal("tar-bytes"))

			m.ResetState(ctx, p.ID)

			Expect(savedState("main")).To(BeEmpty())
			got, err := store.Get(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.StateBytes).To(BeZero())
			Expect(got.StateSavedAt.IsZero()).To(BeTrue())
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

			p, err := mgr.Register(ctx, "proj", "b", previewOrigin{})
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

			p, err := mgr.Register(ctx, "proj", "only", previewOrigin{})
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

			p, err := mgr.Register(ctx, "proj", "b", previewOrigin{})
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
					Sites:     []preview.Site{{Name: "web", Host: ref + ".preview.example.com", Port: 3000}},
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
				Expect(got.Sites).To(HaveLen(1))
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

			p, err := mgr.Register(ctx, "proj", "main", previewOrigin{})
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

// indexOf is where an event sits in a recorded sequence, or -1. It is what lets
// a spec say "the servers stopped before the tar ran" rather than merely that
// both happened.
func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}
