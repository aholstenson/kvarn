package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/vm"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// bootOutcome is what one Create call of bootProvider does.
type bootOutcome int

const (
	// bootRegisters starts a VM whose runner registers.
	bootRegisters bootOutcome = iota
	// bootSilent starts a VM whose runner never registers.
	bootSilent
	// bootCreateFails makes Create return an error.
	bootCreateFails
)

// bootProvider is a vm.Provider that plays one scripted outcome per Create
// call and records which VMs it created and destroyed.
type bootProvider struct {
	registry *dispatch.Registry
	outcomes []bootOutcome

	// onCreate, when set, runs at the start of every Create call.
	onCreate func(attempt int)

	mu        sync.Mutex
	calls     int
	tokens    []string
	created   []string
	destroyed []string
	listeners []net.Listener
}

func (p *bootProvider) Name() string { return "boot-test" }

func (p *bootProvider) PrepareImage(context.Context, vm.BaseImage) (*vm.ProviderImage, error) {
	return &vm.ProviderImage{}, nil
}

func (p *bootProvider) Create(_ context.Context, opts vm.CreateOpts) (*vm.VM, *vm.RunnerConn, error) {
	p.mu.Lock()
	p.calls++
	attempt := p.calls
	p.tokens = append(p.tokens, opts.Token)
	p.mu.Unlock()

	if p.onCreate != nil {
		p.onCreate(attempt)
	}

	outcome := bootRegisters
	if attempt <= len(p.outcomes) {
		outcome = p.outcomes[attempt-1]
	}
	if outcome == bootCreateFails {
		return nil, nil, fmt.Errorf("hypervisor refused VM %d", attempt)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	id := fmt.Sprintf("vm-%d", attempt)
	p.mu.Lock()
	p.created = append(p.created, id)
	p.listeners = append(p.listeners, ln)
	p.mu.Unlock()

	if outcome == bootRegisters {
		pr, ok := p.registry.Lookup(opts.Token)
		if !ok {
			return nil, nil, errors.New("token was not registered before Create")
		}
		go pr.MarkReady()
	}
	return &vm.VM{ID: id}, &vm.RunnerConn{Listener: ln}, nil
}

func (p *bootProvider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, id)
	return nil
}

func (p *bootProvider) List(context.Context) ([]*vm.VM, error) { return nil, nil }

func (p *bootProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *bootProvider) Tokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tokens...)
}

func (p *bootProvider) Created() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.created...)
}

func (p *bootProvider) Destroyed() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.destroyed...)
}

var _ = Describe("VM boot", func() {
	var (
		registry *dispatch.Registry
		provider *bootProvider
		events   []sandbox.Event
		eventsMu sync.Mutex
		opts     sandbox.Opts
	)

	BeforeEach(func() {
		DeferCleanup(sandbox.SetBootRetryForTest(
			[]time.Duration{time.Millisecond, time.Millisecond}, 100*time.Millisecond))

		registry = dispatch.NewRegistry()
		provider = &bootProvider{registry: registry}
		events = nil
		opts = sandbox.Opts{
			Provider: provider,
			OnEvent: func(e sandbox.Event) {
				eventsMu.Lock()
				defer eventsMu.Unlock()
				events = append(events, e)
			},
		}
	})

	consoleOutput := func() []string {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		var out []string
		for _, e := range events {
			if c, ok := e.(sandbox.ConsoleOutputEvent); ok {
				out = append(out, c.Output)
			}
		}
		return out
	}

	It("boots once when the first VM registers", func() {
		id, release, err := sandbox.BootVMForTest(context.Background(), opts, registry)
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal("vm-1"))
		Expect(provider.Calls()).To(Equal(1))
		Expect(provider.Destroyed()).To(BeEmpty())

		release()
		Expect(provider.Destroyed()).To(Equal([]string{"vm-1"}))
		_, stillRegistered := registry.Lookup(provider.Tokens()[0])
		Expect(stillRegistered).To(BeFalse())
	})

	It("boots a new VM when the runner of the first never registers", func() {
		provider.outcomes = []bootOutcome{bootSilent, bootRegisters}

		id, release, err := sandbox.BootVMForTest(context.Background(), opts, registry)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(release)

		Expect(id).To(Equal("vm-2"))
		Expect(provider.Destroyed()).To(Equal([]string{"vm-1"}))

		tokens := provider.Tokens()
		Expect(tokens).To(HaveLen(2))
		Expect(tokens[0]).NotTo(Equal(tokens[1]), "each attempt registers its own token")
		_, staleRegistered := registry.Lookup(tokens[0])
		Expect(staleRegistered).To(BeFalse(), "the discarded VM's token is removed")

		Expect(consoleOutput()).To(ContainElement(
			And(ContainSubstring("VM boot failed"), ContainSubstring("attempt 2 of 3"))))
	})

	It("boots a new VM when Create fails", func() {
		provider.outcomes = []bootOutcome{bootCreateFails, bootCreateFails, bootRegisters}

		id, release, err := sandbox.BootVMForTest(context.Background(), opts, registry)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(release)

		Expect(id).To(Equal("vm-3"))
		Expect(provider.Calls()).To(Equal(3))
	})

	It("reports the last failure once every attempt has failed", func() {
		provider.outcomes = []bootOutcome{bootSilent, bootCreateFails, bootSilent}

		_, _, err := sandbox.BootVMForTest(context.Background(), opts, registry)
		Expect(err).To(MatchError(And(
			ContainSubstring("after 3 attempts"),
			ContainSubstring("timed out after 100ms waiting for runner to connect"))))
		Expect(provider.Calls()).To(Equal(3))
		Expect(provider.Destroyed()).To(ConsistOf(provider.Created()))
		for _, token := range provider.Tokens() {
			_, registered := registry.Lookup(token)
			Expect(registered).To(BeFalse())
		}
	})

	It("does not boot again once the context is cancelled", func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		provider.outcomes = []bootOutcome{bootSilent, bootSilent, bootSilent}
		provider.onCreate = func(int) { cancel() }

		_, _, err := sandbox.BootVMForTest(ctx, opts, registry)
		Expect(err).To(MatchError(context.Canceled))
		Expect(provider.Calls()).To(Equal(1))
		Expect(provider.Destroyed()).To(Equal([]string{"vm-1"}))
	})
})
