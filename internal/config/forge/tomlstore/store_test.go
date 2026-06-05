package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
	generic "github.com/aholstenson/kvarn/internal/config/tomlstore"
)

var _ = Describe("Forge Config TomlStore", func() {
	var (
		store  *tomlstore.Store
		tmpDir string
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "forge-store-test-*")
		Expect(err).NotTo(HaveOccurred())
		store = tomlstore.New(filepath.Join(tmpDir, "forges.toml"))
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("puts and gets a forge config", func() {
		err := store.Put(ctx, &forgeconfig.ForgeConfig{
			Name:       "github-myorg",
			Type:       "github",
			Credential: "myorg-pat",
		})
		Expect(err).NotTo(HaveOccurred())

		fc, err := store.Get(ctx, "github-myorg")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.Name).To(Equal("github-myorg"))
		Expect(fc.Type).To(Equal("github"))
		Expect(fc.Credential).To(Equal("myorg-pat"))
	})

	It("lists forge configs", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "a", Type: "github"})).To(Succeed())
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "b", Type: "git"})).To(Succeed())

		configs, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(configs).To(HaveLen(2))
	})

	It("deletes a forge config", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "deleteme", Type: "git"})).To(Succeed())

		err := store.Delete(ctx, "deleteme")
		Expect(err).NotTo(HaveOccurred())

		_, err = store.Get(ctx, "deleteme")
		Expect(err).To(HaveOccurred())
	})

	It("returns ErrNotFound for missing forge config", func() {
		_, err := store.Get(ctx, "nonexistent")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("returns ErrNotFound when deleting missing forge config", func() {
		err := store.Delete(ctx, "nonexistent")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("stores all fields", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{
			Name:              "full",
			Type:              "github",
			Credential:        "my-cred",
			BranchPrefix:      "automated",
			Labels:            []string{"bot", "auto"},
			CommitAuthorName:  "MyBot",
			CommitAuthorEmail: "bot@myorg.com",
		})).To(Succeed())

		fc, err := store.Get(ctx, "full")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.Type).To(Equal("github"))
		Expect(fc.Credential).To(Equal("my-cred"))
		Expect(fc.BranchPrefix).To(Equal("automated"))
		Expect(fc.Labels).To(Equal([]string{"bot", "auto"}))
		Expect(fc.CommitAuthorName).To(Equal("MyBot"))
		Expect(fc.CommitAuthorEmail).To(Equal("bot@myorg.com"))
	})

	It("updates an existing forge config", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "fc", Type: "git"})).To(Succeed())
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "fc", Type: "github"})).To(Succeed())

		fc, err := store.Get(ctx, "fc")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.Type).To(Equal("github"))
	})

	It("handles missing file gracefully", func() {
		configs, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(configs).To(BeEmpty())
	})

	It("returns zero-value defaults when no [defaults] block is present", func() {
		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "a", Type: "github"})).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(forgeconfig.Defaults{}))
	})

	It("returns zero-value defaults for a missing file", func() {
		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d).To(Equal(forgeconfig.Defaults{}))
	})

	It("parses the [defaults] block", func() {
		path := filepath.Join(tmpDir, "forges.toml")
		content := `[defaults]
branch_prefix       = "bot"
commit_author_name  = "Global Bot"
commit_author_email = "global@example.com"
labels              = ["automated", "kvarn"]

[forges.github-myorg]
type          = "github"
branch_prefix = "myorg"
`
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.BranchPrefix).To(Equal("bot"))
		Expect(d.CommitAuthorName).To(Equal("Global Bot"))
		Expect(d.CommitAuthorEmail).To(Equal("global@example.com"))
		Expect(d.Labels).To(Equal([]string{"automated", "kvarn"}))

		// The named forge override is still readable alongside the defaults.
		fc, err := store.Get(ctx, "github-myorg")
		Expect(err).NotTo(HaveOccurred())
		Expect(fc.BranchPrefix).To(Equal("myorg"))
	})

	It("surfaces a parse error from Defaults rather than silently falling back", func() {
		path := filepath.Join(tmpDir, "forges.toml")
		Expect(os.WriteFile(path, []byte("not = valid = toml"), 0o644)).To(Succeed())

		_, err := store.Defaults(ctx)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse "))
	})

	It("preserves an existing [defaults] block across a Put", func() {
		path := filepath.Join(tmpDir, "forges.toml")
		content := `[defaults]
branch_prefix = "bot"
`
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())

		Expect(store.Put(ctx, &forgeconfig.ForgeConfig{Name: "new", Type: "git"})).To(Succeed())

		d, err := store.Defaults(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(d.BranchPrefix).To(Equal("bot"))
	})
})
