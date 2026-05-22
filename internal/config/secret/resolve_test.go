package secret_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fmt"

	"github.com/aholstenson/kvarn/internal/config/secret"
)

// memStore is a small in-memory secret.Store for testing Resolve.
type memStore struct {
	secrets map[string]map[string]*secret.Secret
}

func newMemStore() *memStore {
	return &memStore{secrets: map[string]map[string]*secret.Secret{}}
}

func (m *memStore) Get(_ context.Context, project, name string) (*secret.Secret, error) {
	if proj, ok := m.secrets[project]; ok {
		if s, ok := proj[name]; ok {
			return s, nil
		}
	}
	return nil, fmt.Errorf("secret %q not found for project %q", name, project)
}

func (m *memStore) List(_ context.Context, project string) ([]*secret.Secret, error) {
	var out []*secret.Secret
	for _, s := range m.secrets[project] {
		out = append(out, s)
	}
	return out, nil
}

func (m *memStore) Put(_ context.Context, s *secret.Secret) error {
	proj, ok := m.secrets[s.Project]
	if !ok {
		proj = map[string]*secret.Secret{}
		m.secrets[s.Project] = proj
	}
	proj[s.Name] = s
	return nil
}

func (m *memStore) Delete(_ context.Context, project, name string) error {
	if proj, ok := m.secrets[project]; ok {
		delete(proj, name)
	}
	return nil
}

var _ = Describe("Resolve", func() {
	var (
		ctx   context.Context
		store *memStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		store = newMemStore()
	})

	It("returns nil maps and nil error when no names are requested", func() {
		env, bearer, err := secret.Resolve(ctx, store, "any", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(BeNil())
		Expect(bearer).To(BeNil())
	})

	It("resolves env-typed secrets as direct env-var values", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "HMAC_SIGN",
			Type: secret.TypeEnv, Value: "real-hmac-value",
		})).To(Succeed())

		env, bearer, err := secret.Resolve(ctx, store, "demo", []string{"HMAC_SIGN"})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(HaveKeyWithValue("HMAC_SIGN", "real-hmac-value"))
		Expect(bearer).To(BeEmpty())
	})

	It("resolves bearer-typed secrets as placeholder→value pairs", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "TOKEN",
			Type: secret.TypeBearer, Value: "real-token",
		})).To(Succeed())

		env, bearer, err := secret.Resolve(ctx, store, "demo", []string{"TOKEN"})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(HaveKey("TOKEN"))
		placeholder := env["TOKEN"]
		Expect(placeholder).To(HavePrefix("kvarn:"))
		Expect(placeholder).NotTo(Equal("real-token"))
		Expect(bearer).To(HaveKeyWithValue(placeholder, "real-token"))
	})

	It("resolves a mix of env and bearer secrets in a single call", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "HMAC_SIGN",
			Type: secret.TypeEnv, Value: "hmac",
		})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "DOCKERHUB",
			Type: secret.TypeBearer, Value: "dh-token",
		})).To(Succeed())

		env, bearer, err := secret.Resolve(ctx, store, "demo",
			[]string{"HMAC_SIGN", "DOCKERHUB"})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(HaveKeyWithValue("HMAC_SIGN", "hmac"))
		Expect(env["DOCKERHUB"]).To(HavePrefix("kvarn:"))
		Expect(bearer).To(HaveKeyWithValue(env["DOCKERHUB"], "dh-token"))
	})

	It("reports every missing name in a single error", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "PRESENT",
			Type: secret.TypeEnv, Value: "v",
		})).To(Succeed())

		_, _, err := secret.Resolve(ctx, store, "demo",
			[]string{"PRESENT", "MISSING_A", "MISSING_B"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("missing secrets for project \"demo\""))
		Expect(err.Error()).To(ContainSubstring("MISSING_A"))
		Expect(err.Error()).To(ContainSubstring("MISSING_B"))
		Expect(err.Error()).NotTo(ContainSubstring("PRESENT"))
	})

	It("errors when names are requested but no store is configured", func() {
		_, _, err := secret.Resolve(ctx, nil, "demo", []string{"X"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no secret store is configured"))
	})

	It("rejects secrets with an unknown type", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "WEIRD",
			Type: "aws", Value: "v",
		})).To(Succeed())

		_, _, err := secret.Resolve(ctx, store, "demo", []string{"WEIRD"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown type"))
	})

	It("uses a fresh placeholder per bearer secret", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "A",
			Type: secret.TypeBearer, Value: "value-a",
		})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "B",
			Type: secret.TypeBearer, Value: "value-b",
		})).To(Succeed())

		env, bearer, err := secret.Resolve(ctx, store, "demo", []string{"A", "B"})
		Expect(err).NotTo(HaveOccurred())
		Expect(env["A"]).NotTo(Equal(env["B"]))
		Expect(bearer).To(HaveLen(2))
	})
})
