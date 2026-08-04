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

var _ = Describe("LLM credentials", func() {
	var (
		store  *tomlstore.Store
		llm    credential.LLMStore
		tmpDir string
		path   string
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "llm-cred-test-*")
		Expect(err).NotTo(HaveOccurred())
		path = filepath.Join(tmpDir, "credentials.toml")
		store = tomlstore.New(path)
		llm = store.LLM()
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("puts and gets a provider credential", func() {
		Expect(llm.Put(ctx, &credential.LLMCredential{
			Provider: "anthropic",
			APIKey:   "sk-ant-123",
		})).To(Succeed())

		cred, err := llm.Get(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.Provider).To(Equal("anthropic"))
		Expect(cred.APIKey).To(Equal("sk-ant-123"))
	})

	It("round-trips extra headers", func() {
		Expect(llm.Put(ctx, &credential.LLMCredential{
			Provider: "openrouter",
			APIKey:   "sk-or-1",
			Headers:  map[string]string{"x-title": "kvarn"},
		})).To(Succeed())

		cred, err := llm.Get(ctx, "openrouter")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.Headers).To(HaveKeyWithValue("x-title", "kvarn"))
	})

	It("returns ErrNotFound for a provider with no credential", func() {
		_, err := llm.Get(ctx, "openai")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("lists providers in name order", func() {
		Expect(llm.Put(ctx, &credential.LLMCredential{Provider: "openai", APIKey: "b"})).To(Succeed())
		Expect(llm.Put(ctx, &credential.LLMCredential{Provider: "anthropic", APIKey: "a"})).To(Succeed())

		creds, err := llm.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(creds).To(HaveLen(2))
		Expect(creds[0].Provider).To(Equal("anthropic"))
		Expect(creds[1].Provider).To(Equal("openai"))
	})

	It("deletes a provider credential", func() {
		Expect(llm.Put(ctx, &credential.LLMCredential{Provider: "google", APIKey: "g"})).To(Succeed())
		Expect(llm.Delete(ctx, "google")).To(Succeed())
		_, err := llm.Get(ctx, "google")
		Expect(err).To(MatchError(generic.ErrNotFound))
	})

	It("writes the file with 0600 permissions", func() {
		Expect(llm.Put(ctx, &credential.LLMCredential{Provider: "anthropic", APIKey: "sk"})).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0600)))
	})

	// The two blocks share one file, so each write must round-trip the other
	// block rather than dropping what it did not read.
	It("keeps forge credentials and LLM credentials independent", func() {
		Expect(store.Put(ctx, &credential.Credential{
			Name:   "github",
			Config: map[string]string{"token": "ghp_1"},
		})).To(Succeed())
		Expect(llm.Put(ctx, &credential.LLMCredential{Provider: "anthropic", APIKey: "sk-ant"})).To(Succeed())
		Expect(store.Put(ctx, &credential.Credential{
			Name:   "gitlab",
			Config: map[string]string{"token": "glpat_1"},
		})).To(Succeed())

		forge, err := store.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(forge).To(HaveLen(2))

		cred, err := llm.Get(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-ant"))

		// A forge credential name matching a provider name stays in its own
		// block; the two namespaces do not see each other.
		Expect(store.Put(ctx, &credential.Credential{
			Name:   "anthropic",
			Config: map[string]string{"token": "not-an-llm-key"},
		})).To(Succeed())

		cred, err = llm.Get(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-ant"))
	})

	It("reads a hand-written file", func() {
		Expect(os.WriteFile(path, []byte(`
[credentials.github]
token = "ghp_x"

[llm.anthropic]
api_key = "sk-ant-hand"

[llm.openrouter]
api_key = "sk-or-hand"
headers = { x-title = "kvarn" }
`), 0o600)).To(Succeed())

		cred, err := llm.Get(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-ant-hand"))

		cred, err = llm.Get(ctx, "openrouter")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.Headers).To(HaveKeyWithValue("x-title", "kvarn"))

		forgeCred, err := store.Get(ctx, "github")
		Expect(err).NotTo(HaveOccurred())
		Expect(forgeCred.Config["token"]).To(Equal("ghp_x"))
	})
})
