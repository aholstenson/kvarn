package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/config/secret"
	"github.com/aholstenson/kvarn/internal/config/secret/tomlstore"
	generic "github.com/aholstenson/kvarn/internal/config/tomlstore"
)

var _ = Describe("Secret TomlStore", func() {
	var (
		store  *tomlstore.Store
		tmpDir string
		path   string
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "secret-store-test-*")
		Expect(err).NotTo(HaveOccurred())
		path = filepath.Join(tmpDir, "secrets.toml")
		store = tomlstore.New(path)
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("puts and gets a secret", func() {
		err := store.Put(ctx, &secret.Secret{
			Project: "my-app",
			Name:    "HMAC_SIGN",
			Type:    secret.TypeEnv,
			Value:   "abc123",
		})
		Expect(err).NotTo(HaveOccurred())

		s, err := store.Get(ctx, "my-app", "HMAC_SIGN")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Project).To(Equal("my-app"))
		Expect(s.Name).To(Equal("HMAC_SIGN"))
		Expect(s.Type).To(Equal(secret.TypeEnv))
		Expect(s.Value).To(Equal("abc123"))
	})

	It("lists secrets for a project sorted by name", func() {
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "B", Type: secret.TypeEnv, Value: "v1"})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "A", Type: secret.TypeBearer, Value: "v2"})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{Project: "other", Name: "X", Type: secret.TypeEnv, Value: "x"})).To(Succeed())

		secrets, err := store.List(ctx, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets).To(HaveLen(2))
		Expect(secrets[0].Name).To(Equal("A"))
		Expect(secrets[1].Name).To(Equal("B"))
	})

	It("returns empty list for unknown project", func() {
		secrets, err := store.List(ctx, "nonexistent")
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets).To(BeEmpty())
	})

	It("deletes a secret", func() {
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "X", Type: secret.TypeEnv, Value: "v"})).To(Succeed())
		Expect(store.Delete(ctx, "a", "X")).To(Succeed())

		_, err := store.Get(ctx, "a", "X")
		Expect(err).To(HaveOccurred())
	})

	It("returns ErrNotFound for missing secret", func() {
		_, err := store.Get(ctx, "a", "X")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("returns ErrNotFound when deleting missing secret", func() {
		err := store.Delete(ctx, "a", "X")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("updates an existing secret", func() {
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "X", Type: secret.TypeEnv, Value: "old"})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "X", Type: secret.TypeBearer, Value: "new"})).To(Succeed())

		s, err := store.Get(ctx, "a", "X")
		Expect(err).NotTo(HaveOccurred())
		Expect(s.Type).To(Equal(secret.TypeBearer))
		Expect(s.Value).To(Equal("new"))
	})

	It("rejects unknown type on Put", func() {
		err := store.Put(ctx, &secret.Secret{Project: "a", Name: "X", Type: "aws", Value: "v"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("invalid secret type"))
	})

	It("creates file with 0600 permissions", func() {
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "X", Type: secret.TypeEnv, Value: "v"})).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("handles missing file gracefully", func() {
		secrets, err := store.List(ctx, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets).To(BeEmpty())
	})

	It("removes empty project map after last secret deleted", func() {
		Expect(store.Put(ctx, &secret.Secret{Project: "a", Name: "X", Type: secret.TypeEnv, Value: "v"})).To(Succeed())
		Expect(store.Delete(ctx, "a", "X")).To(Succeed())

		secrets, err := store.List(ctx, "a")
		Expect(err).NotTo(HaveOccurred())
		Expect(secrets).To(BeEmpty())
	})
})
