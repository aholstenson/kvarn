package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/config/project"
	"github.com/aholstenson/kvarn/internal/config/project/tomlstore"
)

var _ = Describe("Project TomlStore", func() {
	var (
		store  *tomlstore.Store
		tmpDir string
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "proj-store-test-*")
		Expect(err).NotTo(HaveOccurred())
		store = tomlstore.New(filepath.Join(tmpDir, "projects.toml"))
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("puts and gets a project", func() {
		err := store.Put(ctx, &project.Project{
			Name:          "my-app",
			RepoURL:       "myorg/my-app",
			DefaultBranch: "main",
			Forge:         "github-myorg",
		})
		Expect(err).NotTo(HaveOccurred())

		proj, err := store.Get(ctx, "my-app")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.Name).To(Equal("my-app"))
		Expect(proj.RepoURL).To(Equal("myorg/my-app"))
		Expect(proj.DefaultBranch).To(Equal("main"))
		Expect(proj.Forge).To(Equal("github-myorg"))
	})

	It("lists projects", func() {
		Expect(store.Put(ctx, &project.Project{Name: "a", RepoURL: "org/a"})).To(Succeed())
		Expect(store.Put(ctx, &project.Project{Name: "b", RepoURL: "org/b"})).To(Succeed())

		projs, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(projs).To(HaveLen(2))
	})

	It("deletes a project", func() {
		Expect(store.Put(ctx, &project.Project{Name: "deleteme", RepoURL: "org/deleteme"})).To(Succeed())

		err := store.Delete(ctx, "deleteme")
		Expect(err).NotTo(HaveOccurred())

		_, err = store.Get(ctx, "deleteme")
		Expect(err).To(HaveOccurred())
	})

	It("returns error for missing project", func() {
		_, err := store.Get(ctx, "nonexistent")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
	})

	It("returns error when deleting missing project", func() {
		err := store.Delete(ctx, "nonexistent")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not found"))
	})

	It("stores forge reference", func() {
		Expect(store.Put(ctx, &project.Project{
			Name:    "app",
			RepoURL: "org/app",
			Forge:   "my-forge",
		})).To(Succeed())

		proj, err := store.Get(ctx, "app")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.Forge).To(Equal("my-forge"))
	})

	It("round-trips per-project PR behavior overrides", func() {
		Expect(store.Put(ctx, &project.Project{
			Name:              "app",
			RepoURL:           "org/app",
			Forge:             "github",
			BranchPrefix:      "agent",
			Labels:            []string{"automated", "needs-review"},
			CommitAuthorName:  "Project Bot",
			CommitAuthorEmail: "bot@example.com",
		})).To(Succeed())

		proj, err := store.Get(ctx, "app")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.BranchPrefix).To(Equal("agent"))
		Expect(proj.Labels).To(Equal([]string{"automated", "needs-review"}))
		Expect(proj.CommitAuthorName).To(Equal("Project Bot"))
		Expect(proj.CommitAuthorEmail).To(Equal("bot@example.com"))
	})

	It("leaves PR behavior overrides empty when unset", func() {
		Expect(store.Put(ctx, &project.Project{Name: "app", RepoURL: "org/app"})).To(Succeed())

		proj, err := store.Get(ctx, "app")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.BranchPrefix).To(BeEmpty())
		Expect(proj.Labels).To(BeEmpty())
		Expect(proj.CommitAuthorName).To(BeEmpty())
		Expect(proj.CommitAuthorEmail).To(BeEmpty())
	})

	It("round-trips clone_depth override", func() {
		depth := 250
		Expect(store.Put(ctx, &project.Project{
			Name:       "deep",
			RepoURL:    "org/deep",
			CloneDepth: &depth,
		})).To(Succeed())

		proj, err := store.Get(ctx, "deep")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.CloneDepth).NotTo(BeNil())
		Expect(*proj.CloneDepth).To(Equal(250))

		full := 0
		Expect(store.Put(ctx, &project.Project{
			Name:       "full",
			RepoURL:    "org/full",
			CloneDepth: &full,
		})).To(Succeed())

		proj, err = store.Get(ctx, "full")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.CloneDepth).NotTo(BeNil())
		Expect(*proj.CloneDepth).To(Equal(0))
	})

	It("leaves clone_depth nil when unset", func() {
		Expect(store.Put(ctx, &project.Project{Name: "app", RepoURL: "org/app"})).To(Succeed())

		proj, err := store.Get(ctx, "app")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.CloneDepth).To(BeNil())
	})

	It("handles missing file gracefully", func() {
		projs, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(projs).To(BeEmpty())
	})

	It("updates an existing project", func() {
		Expect(store.Put(ctx, &project.Project{Name: "app", RepoURL: "org/old"})).To(Succeed())
		Expect(store.Put(ctx, &project.Project{Name: "app", RepoURL: "org/new"})).To(Succeed())

		proj, err := store.Get(ctx, "app")
		Expect(err).NotTo(HaveOccurred())
		Expect(proj.RepoURL).To(Equal("org/new"))
	})
})
