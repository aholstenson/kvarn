package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/config/tomlstore"
)

// thingEntry / thingFile / thing exercise the generic store with the minimal
// shape it supports: a flat map of named entries adapted to a domain struct.

type thingEntry struct {
	Value string `toml:"value"`
}

type thingFile struct {
	Things map[string]thingEntry `toml:"things"`
}

type thing struct {
	Name  string
	Value string
}

func newThingStore(path string, mode tomlstore.Mode) *tomlstore.Store[string, thingFile, thingEntry, thing] {
	return tomlstore.New(
		path,
		mode,
		tomlstore.Schema[string, thingFile, thingEntry]{
			NewFileData: func() thingFile { return thingFile{Things: map[string]thingEntry{}} },
			Get: func(fd thingFile, k string) (thingEntry, bool) {
				e, ok := fd.Things[k]
				return e, ok
			},
			Put: func(fd *thingFile, k string, e thingEntry) {
				if fd.Things == nil {
					fd.Things = map[string]thingEntry{}
				}
				fd.Things[k] = e
			},
			Delete: func(fd *thingFile, k string) bool {
				if _, ok := fd.Things[k]; !ok {
					return false
				}
				delete(fd.Things, k)
				return true
			},
			Keys: func(fd thingFile) []string {
				ks := make([]string, 0, len(fd.Things))
				for k := range fd.Things {
					ks = append(ks, k)
				}
				return ks
			},
			Less: func(a, b string) bool { return a < b },
		},
		func(k string, e thingEntry) (thing, error) {
			return thing{Name: k, Value: e.Value}, nil
		},
		func(t thing) (string, thingEntry) {
			return t.Name, thingEntry{Value: t.Value}
		},
	)
}

var _ = Describe("Store", func() {
	var (
		ctx   context.Context
		dir   string
		path  string
		store *tomlstore.Store[string, thingFile, thingEntry, thing]
	)

	BeforeEach(func() {
		ctx = context.Background()
		dir = GinkgoT().TempDir()
		path = filepath.Join(dir, "things.toml")
		store = newThingStore(path, tomlstore.Config)
	})

	It("returns an empty, non-nil list when the file does not exist", func() {
		out, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(BeNil())
		Expect(out).To(BeEmpty())
	})

	It("returns ErrNotFound for a missing entry", func() {
		_, err := store.Get(ctx, "nope")
		Expect(err).To(MatchError(tomlstore.ErrNotFound))
	})

	It("round-trips a domain value through Put/Get", func() {
		Expect(store.Put(ctx, thing{Name: "a", Value: "alpha"})).To(Succeed())

		out, err := store.Get(ctx, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal(thing{Name: "a", Value: "alpha"}))
	})

	It("lists entries in canonical sort order", func() {
		for _, n := range []string{"c", "a", "b"} {
			Expect(store.Put(ctx, thing{Name: n, Value: n + "!"})).To(Succeed())
		}
		out, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(Equal([]thing{
			{Name: "a", Value: "a!"},
			{Name: "b", Value: "b!"},
			{Name: "c", Value: "c!"},
		}))
	})

	It("deletes an entry and returns ErrNotFound for an unknown delete", func() {
		Expect(store.Put(ctx, thing{Name: "a", Value: "x"})).To(Succeed())
		Expect(store.Delete(ctx, "a")).To(Succeed())

		_, err := store.Get(ctx, "a")
		Expect(err).To(MatchError(tomlstore.ErrNotFound))

		Expect(store.Delete(ctx, "a")).To(MatchError(tomlstore.ErrNotFound))
	})

	It("bubbles parse errors with the file path", func() {
		Expect(os.WriteFile(path, []byte("not = valid = toml"), 0o644)).To(Succeed())

		_, err := store.List(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse "))
		Expect(err.Error()).To(ContainSubstring(path))
	})

	It("writes the file with Config mode (0644)", func() {
		Expect(store.Put(ctx, thing{Name: "a", Value: "x"})).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("writes the file with Secret mode (0600) when so configured", func() {
		secretPath := filepath.Join(dir, "secret.toml")
		secretStore := newThingStore(secretPath, tomlstore.Secret)
		Expect(secretStore.Put(ctx, thing{Name: "a", Value: "x"})).To(Succeed())

		info, err := os.Stat(secretPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("creates the parent directory on first write", func() {
		nested := filepath.Join(dir, "sub", "nested", "things.toml")
		nestedStore := newThingStore(nested, tomlstore.Config)
		Expect(nestedStore.Put(ctx, thing{Name: "a", Value: "x"})).To(Succeed())

		info, err := os.Stat(nested)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("Load returns the parsed FD verbatim", func() {
		Expect(store.Put(ctx, thing{Name: "a", Value: "alpha"})).To(Succeed())

		fd, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(fd.Things).To(HaveKey("a"))
		Expect(fd.Things["a"].Value).To(Equal("alpha"))
	})

	It("Load returns a fresh FD for a missing file", func() {
		fd, err := store.Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(fd.Things).NotTo(BeNil())
		Expect(fd.Things).To(BeEmpty())
	})
})
