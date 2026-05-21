package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	forgeconfig "github.com/aholstenson/kvarn/internal/config/forge"
	"github.com/aholstenson/kvarn/internal/config/forge/tomlstore"
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

	It("returns error for missing forge config", func() {
		_, err := store.Get(ctx, "nonexistent")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
	})

	It("returns error when deleting missing forge config", func() {
		err := store.Delete(ctx, "nonexistent")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
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
})
