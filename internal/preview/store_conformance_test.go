package preview_test

import (
	"context"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/preview"
	sqlitestore "github.com/aholstenson/kvarn/internal/preview/sqlite"
)

// makePreview builds a preview with deterministic timestamps for store tests.
func makePreview(id, project, ref string, state preview.State, at time.Time, hosts ...string) *preview.Preview {
	sites := make([]preview.Site, 0, len(hosts))
	for i, host := range hosts {
		sites = append(sites, preview.Site{Name: "site" + string(rune('a'+i)), Host: host, Port: uint16(3000 + i)})
	}
	return &preview.Preview{
		ID:        id,
		Project:   project,
		Ref:       ref,
		State:     state,
		Sites:     sites,
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func idsOf(previews []*preview.Preview) []string {
	out := make([]string, len(previews))
	for i, p := range previews {
		out[i] = p.ID
	}
	return out
}

// DescribeStore runs the shared Store conformance suite against whatever the
// factory produces, so the in-memory store used by manager specs and the
// SQLite store used in production cannot drift apart.
func DescribeStore(name string, newStore func() preview.Store) bool {
	return Describe("Store conformance: "+name, func() {
		var (
			store preview.Store
			ctx   context.Context
			base  time.Time
		)

		BeforeEach(func() {
			store = newStore()
			ctx = context.Background()
			base = time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
			DeferCleanup(func() { Expect(store.Close()).To(Succeed()) })
		})

		It("round-trips a preview with its apps", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateRunning, base,
				"main.preview.example.com", "assets-main.preview.example.com")
			p.SessionID = "sess-1"
			p.StartedAt = base.Add(time.Minute)
			p.LastRequestAt = base.Add(2 * time.Minute)
			p.ExpiresAt = base.Add(8 * time.Hour)
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := store.Get(ctx, "proj/main")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Project).To(Equal("proj"))
			Expect(got.Ref).To(Equal("main"))
			Expect(got.State).To(Equal(preview.StateRunning))
			Expect(got.SessionID).To(Equal("sess-1"))
			Expect(got.Sites).To(HaveLen(2))
			Expect(got.Sites[0].Host).To(Equal("main.preview.example.com"))
			Expect(got.Sites[0].Port).To(Equal(uint16(3000)))
			Expect(got.Sites[1].Port).To(Equal(uint16(3001)))
			Expect(got.CreatedAt).To(BeTemporally("==", base))
			Expect(got.StartedAt).To(BeTemporally("==", base.Add(time.Minute)))
			Expect(got.LastRequestAt).To(BeTemporally("==", base.Add(2*time.Minute)))
			Expect(got.ExpiresAt).To(BeTemporally("==", base.Add(8*time.Hour)))
		})

		It("leaves an unset timestamp unset rather than at the epoch", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateStopped, base, "main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := store.Get(ctx, "proj/main")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.StartedAt.IsZero()).To(BeTrue())
			Expect(got.LastRequestAt.IsZero()).To(BeTrue())
			Expect(got.ExpiresAt.IsZero()).To(BeTrue())
		})

		It("reports ErrNotFound for an unknown preview", func() {
			_, err := store.Get(ctx, "nope")
			Expect(err).To(MatchError(preview.ErrNotFound))
		})

		It("remembers which pull request a preview is of and which name asked for it", func() {
			p := makePreview("proj/feature", "proj", "feature", preview.StateStopped, base,
				"pr-12.preview.example.com")
			p.PR = "12"
			p.AutoStartHost = "PR-12.Preview.Example.Com"
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := store.Get(ctx, "proj/feature")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.PR).To(Equal("12"))
			// Normalized on the way in, so it can be compared with a Host header.
			Expect(got.AutoStartHost).To(Equal("pr-12.preview.example.com"))
			Expect(got.AutoStarted()).To(BeTrue())
		})

		It("reports a preview nobody auto-started as such", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateStopped, base,
				"main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := store.Get(ctx, "proj/main")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.PR).To(BeEmpty())
			Expect(got.AutoStarted()).To(BeFalse())
		})

		It("updates an existing preview in place", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateBooting, base, "main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())

			p.State = preview.StateRunning
			p.UpdatedAt = base.Add(time.Minute)
			Expect(store.Put(ctx, p)).To(Succeed())

			all, err := store.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(all).To(HaveLen(1))
			Expect(all[0].State).To(Equal(preview.StateRunning))
		})

		It("finds a preview by any of its hostnames", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateRunning, base,
				"main.preview.example.com", "assets-main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())

			for _, host := range []string{"main.preview.example.com", "assets-main.preview.example.com"} {
				got, err := store.FindByHost(ctx, host)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.ID).To(Equal("proj/main"))
			}
		})

		It("normalizes the host on lookup", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateRunning, base, "main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())

			for _, spelling := range []string{
				"MAIN.preview.example.com",
				"main.preview.example.com:8080",
				"main.preview.example.com.",
				"  main.preview.example.com  ",
			} {
				got, err := store.FindByHost(ctx, spelling)
				Expect(err).NotTo(HaveOccurred(), spelling)
				Expect(got.ID).To(Equal("proj/main"), spelling)
			}
		})

		It("reports ErrNotFound for a hostname nothing claims", func() {
			_, err := store.FindByHost(ctx, "nobody.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNotFound))
		})

		It("refuses a hostname another preview already claims", func() {
			first := makePreview("proj/main", "proj", "main", preview.StateRunning, base, "shared.preview.example.com")
			Expect(store.Put(ctx, first)).To(Succeed())

			second := makePreview("proj/other", "proj", "other", preview.StateStopped, base, "shared.preview.example.com")
			Expect(store.Put(ctx, second)).To(MatchError(preview.ErrHostTaken))

			// The rejected write left nothing behind.
			_, err := store.Get(ctx, "proj/other")
			Expect(err).To(MatchError(preview.ErrNotFound))
			got, err := store.FindByHost(ctx, "shared.preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.ID).To(Equal("proj/main"))
		})

		It("lets a preview keep its own hostnames across writes", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateBooting, base, "main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())
			p.State = preview.StateRunning
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := store.FindByHost(ctx, "main.preview.example.com")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.State).To(Equal(preview.StateRunning))
		})

		It("releases a hostname a preview no longer serves", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateRunning, base,
				"main.preview.example.com", "assets-main.preview.example.com")
			Expect(store.Put(ctx, p)).To(Succeed())

			p.Sites = p.Sites[:1]
			Expect(store.Put(ctx, p)).To(Succeed())

			_, err := store.FindByHost(ctx, "assets-main.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNotFound))
			_, err = store.FindByHost(ctx, "main.preview.example.com")
			Expect(err).NotTo(HaveOccurred())
		})

		It("lists previews ordered by ID", func() {
			Expect(store.Put(ctx, makePreview("b/x", "b", "x", preview.StateStopped, base, "x.b.example.com"))).To(Succeed())
			Expect(store.Put(ctx, makePreview("a/y", "a", "y", preview.StateStopped, base, "y.a.example.com"))).To(Succeed())
			Expect(store.Put(ctx, makePreview("c/z", "c", "z", preview.StateStopped, base, "z.c.example.com"))).To(Succeed())

			all, err := store.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(idsOf(all)).To(Equal([]string{"a/y", "b/x", "c/z"}))
		})

		It("returns an empty listing rather than an error when there is nothing", func() {
			all, err := store.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(all).To(BeEmpty())
		})

		It("deletes a preview and frees its hostnames", func() {
			Expect(store.Put(ctx, makePreview("proj/main", "proj", "main", preview.StateStopped, base,
				"main.preview.example.com"))).To(Succeed())
			Expect(store.Delete(ctx, "proj/main")).To(Succeed())

			_, err := store.Get(ctx, "proj/main")
			Expect(err).To(MatchError(preview.ErrNotFound))
			_, err = store.FindByHost(ctx, "main.preview.example.com")
			Expect(err).To(MatchError(preview.ErrNotFound))

			// The name is available again.
			Expect(store.Put(ctx, makePreview("proj/other", "proj", "other", preview.StateStopped, base,
				"main.preview.example.com"))).To(Succeed())
		})

		It("reports ErrNotFound when deleting an unknown preview", func() {
			Expect(store.Delete(ctx, "nope")).To(MatchError(preview.ErrNotFound))
		})

		It("stamps the last-request time", func() {
			Expect(store.Put(ctx, makePreview("proj/main", "proj", "main", preview.StateRunning, base,
				"main.preview.example.com"))).To(Succeed())

			at := base.Add(3 * time.Minute)
			Expect(store.TouchRequest(ctx, "proj/main", at)).To(Succeed())

			got, err := store.Get(ctx, "proj/main")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.LastRequestAt).To(BeTemporally("==", at))
		})

		It("ignores a touch for a preview that is gone", func() {
			Expect(store.TouchRequest(ctx, "nope", base)).To(Succeed())
		})

		It("resets live previews to stopped and clears their VM fields", func() {
			booting := makePreview("proj/a", "proj", "a", preview.StateBooting, base, "a.preview.example.com")
			booting.SessionID = "sess-a"
			booting.StartedAt = base
			booting.ExpiresAt = base.Add(time.Hour)
			Expect(store.Put(ctx, booting)).To(Succeed())

			running := makePreview("proj/b", "proj", "b", preview.StateRunning, base, "b.preview.example.com")
			running.SessionID = "sess-b"
			running.StartedAt = base
			Expect(store.Put(ctx, running)).To(Succeed())

			stopped := makePreview("proj/c", "proj", "c", preview.StateStopped, base, "c.preview.example.com")
			stopped.LastRequestAt = base
			Expect(store.Put(ctx, stopped)).To(Succeed())

			reset, err := store.ResetLive(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(reset).To(ConsistOf("proj/a", "proj/b"))

			for _, id := range []string{"proj/a", "proj/b"} {
				got, err := store.Get(ctx, id)
				Expect(err).NotTo(HaveOccurred())
				Expect(got.State).To(Equal(preview.StateStopped))
				Expect(got.StartedAt.IsZero()).To(BeTrue())
				Expect(got.ExpiresAt.IsZero()).To(BeTrue())
				Expect(got.SessionID).To(BeEmpty())
			}

			// A stopped preview keeps everything it had, including the
			// last-request stamp eviction ordering depends on.
			got, err := store.Get(ctx, "proj/c")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.LastRequestAt).To(BeTemporally("==", base))
		})

		It("resets a failed preview too, so the next request can boot it", func() {
			failed := makePreview("proj/a", "proj", "a", preview.StateFailed, base, "a.preview.example.com")
			failed.Error = "setup step failed"
			Expect(store.Put(ctx, failed)).To(Succeed())

			reset, err := store.ResetLive(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(reset).To(ConsistOf("proj/a"))
		})

		It("reports nothing to reset when everything is already stopped", func() {
			Expect(store.Put(ctx, makePreview("proj/a", "proj", "a", preview.StateStopped, base,
				"a.preview.example.com"))).To(Succeed())

			reset, err := store.ResetLive(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(reset).To(BeEmpty())
		})

		It("keeps a preview with no apps addressable by ID", func() {
			p := makePreview("proj/main", "proj", "main", preview.StateStopped, base)
			Expect(store.Put(ctx, p)).To(Succeed())

			got, err := store.Get(ctx, "proj/main")
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Sites).To(BeEmpty())
		})
	})
}

var _ = DescribeStore("memstore", func() preview.Store {
	return preview.NewMemStore()
})

var _ = DescribeStore("sqlite", func() preview.Store {
	path := filepath.Join(GinkgoT().TempDir(), "previews.db")
	store, err := sqlitestore.New(path)
	Expect(err).NotTo(HaveOccurred())
	return store
})
