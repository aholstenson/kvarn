package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/apikey/tomlstore"
	generic "github.com/aholstenson/kvarn/internal/config/tomlstore"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Store", func() {
	var (
		ctx   context.Context
		dir   string
		path  string
		store *tomlstore.Store
	)

	BeforeEach(func() {
		ctx = context.Background()
		dir = GinkgoT().TempDir()
		path = filepath.Join(dir, "apikeys.toml")
		store = tomlstore.New(path)
	})

	It("returns ErrNotFound for a missing key", func() {
		_, err := store.Get(ctx, "nope")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("returns an empty list when the file does not exist", func() {
		keys, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(keys).To(BeEmpty())
	})

	It("round-trips a key through Put/Get", func() {
		expires := time.Date(2027, 6, 1, 12, 0, 0, 0, time.UTC)
		in := &apikey.APIKey{
			KeyID:    "abc123",
			Name:     "ci",
			Hash:     "deadbeef",
			Projects: []string{"proj-a", "proj-b"},
			Created:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Expires:  &expires,
		}
		Expect(store.Put(ctx, in)).To(Succeed())

		out, err := store.Get(ctx, "abc123")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.KeyID).To(Equal("abc123"))
		Expect(out.Name).To(Equal("ci"))
		Expect(out.Hash).To(Equal("deadbeef"))
		Expect(out.Projects).To(Equal([]string{"proj-a", "proj-b"}))
		Expect(out.Created).To(BeTemporally("==", in.Created))
		Expect(out.Expires).NotTo(BeNil())
		Expect(*out.Expires).To(BeTemporally("==", expires))
		Expect(out.Disabled).To(BeFalse())
	})

	It("persists across a fresh Store on the same path", func() {
		Expect(store.Put(ctx, &apikey.APIKey{
			KeyID: "k1", Name: "one", Hash: "h1", Projects: []string{"*"},
			Created: time.Now().UTC(),
		})).To(Succeed())

		reopened := tomlstore.New(path)
		out, err := reopened.Get(ctx, "k1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Name).To(Equal("one"))
	})

	It("lists keys sorted by key ID", func() {
		for _, id := range []string{"ccc", "aaa", "bbb"} {
			Expect(store.Put(ctx, &apikey.APIKey{
				KeyID: id, Name: id, Hash: "h", Projects: []string{"*"},
				Created: time.Now().UTC(),
			})).To(Succeed())
		}
		keys, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(keys).To(HaveLen(3))
		Expect([]string{keys[0].KeyID, keys[1].KeyID, keys[2].KeyID}).
			To(Equal([]string{"aaa", "bbb", "ccc"}))
	})

	It("updates an existing key in place", func() {
		k := &apikey.APIKey{
			KeyID: "k1", Name: "one", Hash: "h1", Projects: []string{"*"},
			Created: time.Now().UTC(),
		}
		Expect(store.Put(ctx, k)).To(Succeed())
		k.Disabled = true
		Expect(store.Put(ctx, k)).To(Succeed())

		out, err := store.Get(ctx, "k1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Disabled).To(BeTrue())
	})

	It("deletes a key and reports ErrNotFound for an unknown delete", func() {
		Expect(store.Put(ctx, &apikey.APIKey{
			KeyID: "k1", Name: "one", Hash: "h1", Projects: []string{"*"},
			Created: time.Now().UTC(),
		})).To(Succeed())
		Expect(store.Delete(ctx, "k1")).To(Succeed())
		_, err := store.Get(ctx, "k1")
		Expect(err).To(MatchError(generic.ErrNotFound))

		Expect(store.Delete(ctx, "missing")).To(MatchError(generic.ErrNotFound))
	})

	It("writes the file with 0600 mode", func() {
		Expect(store.Put(ctx, &apikey.APIKey{
			KeyID: "k1", Name: "one", Hash: "h1", Projects: []string{"*"},
			Created: time.Now().UTC(),
		})).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})
})
