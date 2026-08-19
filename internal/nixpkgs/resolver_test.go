package nixpkgs_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/nixpkgs"
	"github.com/aholstenson/kvarn/internal/project"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	revA = "1111111111111111111111111111111111111111"
	revB = "2222222222222222222222222222222222222222"
)

// fakeClock advances only when a spec says so, so TTL expiry is exercised
// without waiting for one.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var _ = Describe("Resolver", func() {
	var (
		ctx   context.Context
		clock *fakeClock
	)

	BeforeEach(func() {
		ctx = context.Background()
		clock = &fakeClock{t: time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)}
	})

	It("resolves a channel to the commit the lookup reports", func() {
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				return revA, nil
			},
		})
		Expect(r.Rev(ctx, project.DefaultNixpkgsChannel)).To(Equal(revA))
	})

	It("reuses a resolved commit until the TTL has passed", func() {
		var calls int
		r := nixpkgs.New(nixpkgs.Options{
			TTL: time.Hour,
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				calls++
				if calls == 1 {
					return revA, nil
				}
				return revB, nil
			},
		})

		Expect(r.Rev(ctx, "nixos-26.05")).To(Equal(revA))
		clock.Advance(59 * time.Minute)
		Expect(r.Rev(ctx, "nixos-26.05")).To(Equal(revA))
		Expect(calls).To(Equal(1))

		clock.Advance(2 * time.Minute)
		Expect(r.Rev(ctx, "nixos-26.05")).To(Equal(revB))
		Expect(calls).To(Equal(2))
	})

	It("keeps one cache entry per channel", func() {
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(_ context.Context, channel string) (string, error) {
				if channel == "nixos-unstable" {
					return revB, nil
				}
				return revA, nil
			},
		})
		Expect(r.Rev(ctx, "nixos-26.05")).To(Equal(revA))
		Expect(r.Rev(ctx, "nixos-unstable")).To(Equal(revB))
	})

	It("serves the last commit it resolved when a later lookup fails", func() {
		var calls int
		r := nixpkgs.New(nixpkgs.Options{
			TTL: time.Hour,
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				calls++
				if calls == 1 {
					return revA, nil
				}
				return "", errors.New("gateway timeout")
			},
		})

		Expect(r.Rev(ctx, "nixos-26.05")).To(Equal(revA))
		clock.Advance(2 * time.Hour)
		Expect(r.Rev(ctx, "nixos-26.05")).To(Equal(revA))
	})

	It("falls back to the compiled-in commit for the default channel", func() {
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				return "", errors.New("gateway timeout")
			},
		})
		Expect(r.Rev(ctx, project.DefaultNixpkgsChannel)).To(Equal(project.DefaultNixpkgsRev))
	})

	It("leaves another channel to Nix when it cannot be resolved", func() {
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				return "", errors.New("gateway timeout")
			},
		})
		Expect(r.Rev(ctx, "nixos-unstable")).To(BeEmpty())
	})

	It("refuses an answer that is not a commit", func() {
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				return "refs/heads/nixos-26.05", nil
			},
		})
		Expect(r.Rev(ctx, "nixos-unstable")).To(BeEmpty())
	})

	It("makes one lookup for concurrent callers on the same channel", func() {
		var mu sync.Mutex
		calls := 0
		release := make(chan struct{})
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				<-release
				return revA, nil
			},
		})

		const callers = 8
		revs := make([]string, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				revs[i] = r.Rev(ctx, "nixos-26.05")
			}()
		}
		// Let every caller reach the resolver before the lookup answers.
		Eventually(func() int { mu.Lock(); defer mu.Unlock(); return calls }).Should(Equal(1))
		close(release)
		wg.Wait()

		Expect(revs).To(HaveEach(revA))
		mu.Lock()
		defer mu.Unlock()
		Expect(calls).To(Equal(1))
	})

	It("resolves nothing for an empty channel", func() {
		r := nixpkgs.New(nixpkgs.Options{
			Now: clock.Now,
			Lookup: func(context.Context, string) (string, error) {
				Fail("lookup should not run for an empty channel")
				return "", nil
			},
		})
		Expect(r.Rev(ctx, "")).To(BeEmpty())
	})
})
