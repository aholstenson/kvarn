package tomlstore_test

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/apikey/tomlstore"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Store capabilities", func() {
	var (
		ctx   context.Context
		path  string
		store *tomlstore.Store
	)

	BeforeEach(func() {
		ctx = context.Background()
		path = filepath.Join(GinkgoT().TempDir(), "apikeys.toml")
		store = tomlstore.New(path)
	})

	It("round-trips capabilities through Put/Get", func() {
		Expect(store.Put(ctx, &apikey.APIKey{
			KeyID: "k1", Name: "ops", Hash: "h1", Projects: []string{"*"},
			Capabilities: []apikey.Capability{apikey.CapabilityHost},
			Created:      time.Now().UTC(),
		})).To(Succeed())

		out, err := store.Get(ctx, "k1")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Capabilities).To(Equal([]apikey.Capability{apikey.CapabilityHost}))
		Expect(out.HasCapability(apikey.CapabilityHost)).To(BeTrue())
	})

	It("reads a key with no capabilities as holding none", func() {
		Expect(store.Put(ctx, &apikey.APIKey{
			KeyID: "k2", Name: "ci", Hash: "h2", Projects: []string{"*"},
			Created: time.Now().UTC(),
		})).To(Succeed())

		out, err := store.Get(ctx, "k2")
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Capabilities).To(BeEmpty())
		Expect(out.HasCapability(apikey.CapabilityHost)).To(BeFalse())
	})

	// Fail closed and loudly: a key that quietly lost the authority its file
	// says it has would only reveal that when an operator needed it to work.
	It("fails the read on an unknown capability rather than dropping it", func() {
		Expect(os.WriteFile(path, []byte(`
[k3]
name = "typo"
hash = "h3"
projects = ["*"]
capabilities = ["hosts"]
created = 2026-01-01T00:00:00Z
`), 0o600)).To(Succeed())

		_, err := store.Get(ctx, "k3")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hosts"))
	})
})
