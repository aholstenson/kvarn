package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/config/credential"
	"github.com/aholstenson/kvarn/internal/config/credential/tomlstore"
	generic "github.com/aholstenson/kvarn/internal/config/tomlstore"
)

var _ = Describe("Credential TomlStore", func() {
	var (
		store  *tomlstore.Store
		tmpDir string
		path   string
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "cred-store-test-*")
		Expect(err).NotTo(HaveOccurred())
		path = filepath.Join(tmpDir, "credentials.toml")
		store = tomlstore.New(path)
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("puts and gets a credential", func() {
		err := store.Put(ctx, &credential.Credential{
			Name:   "github",
			Config: map[string]string{"token": "ghp_abc123"},
		})
		Expect(err).NotTo(HaveOccurred())

		cred, err := store.Get(ctx, "github")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.Name).To(Equal("github"))
		Expect(cred.Config["token"]).To(Equal("ghp_abc123"))
	})

	It("lists credentials", func() {
		Expect(store.Put(ctx, &credential.Credential{Name: "a", Config: map[string]string{"token": "t1"}})).To(Succeed())
		Expect(store.Put(ctx, &credential.Credential{Name: "b", Config: map[string]string{"token": "t2"}})).To(Succeed())

		creds, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(creds).To(HaveLen(2))
	})

	It("deletes a credential", func() {
		Expect(store.Put(ctx, &credential.Credential{Name: "deleteme", Config: map[string]string{"token": "x"}})).To(Succeed())

		err := store.Delete(ctx, "deleteme")
		Expect(err).NotTo(HaveOccurred())

		_, err = store.Get(ctx, "deleteme")
		Expect(err).To(HaveOccurred())
	})

	It("returns ErrNotFound for missing credential", func() {
		_, err := store.Get(ctx, "nonexistent")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("returns ErrNotFound when deleting missing credential", func() {
		err := store.Delete(ctx, "nonexistent")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("updates an existing credential", func() {
		Expect(store.Put(ctx, &credential.Credential{Name: "github", Config: map[string]string{"token": "old"}})).To(Succeed())
		Expect(store.Put(ctx, &credential.Credential{Name: "github", Config: map[string]string{"token": "new"}})).To(Succeed())

		cred, err := store.Get(ctx, "github")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.Config["token"]).To(Equal("new"))
	})

	It("creates file with 0600 permissions", func() {
		Expect(store.Put(ctx, &credential.Credential{Name: "test", Config: map[string]string{"token": "secret"}})).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0600)))
	})

	It("handles missing file gracefully", func() {
		creds, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(creds).To(BeEmpty())
	})

	It("stores arbitrary config fields", func() {
		Expect(store.Put(ctx, &credential.Credential{
			Name: "full",
			Config: map[string]string{
				"token":           "tok",
				"ssh_key_path":    "/path/to/key",
				"ssh_key_pass":    "pass",
				"username":        "user",
				"password":        "pw",
				"app_id":          "12345",
				"installation_id": "67890",
			},
		})).To(Succeed())

		cred, err := store.Get(ctx, "full")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.Config["token"]).To(Equal("tok"))
		Expect(cred.Config["ssh_key_path"]).To(Equal("/path/to/key"))
		Expect(cred.Config["ssh_key_pass"]).To(Equal("pass"))
		Expect(cred.Config["username"]).To(Equal("user"))
		Expect(cred.Config["password"]).To(Equal("pw"))
		Expect(cred.Config["app_id"]).To(Equal("12345"))
		Expect(cred.Config["installation_id"]).To(Equal("67890"))
	})
})
