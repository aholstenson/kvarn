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

	It("returns nil maps and nil error when no refs are requested", func() {
		env, managed, err := secret.Resolve(ctx, store, "any", nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(BeNil())
		Expect(managed).To(BeNil())
	})

	It("resolves env-typed secrets as direct env-var values", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "HMAC_SIGN",
			Type: secret.TypeEnv, Value: "real-hmac-value",
		})).To(Succeed())

		env, managed, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{Name: "HMAC_SIGN"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(HaveKeyWithValue("HMAC_SIGN", "real-hmac-value"))
		Expect(managed).To(BeEmpty())
	})

	It("resolves managed secrets as placeholder→value pairs", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "TOKEN",
			Type: secret.TypeManaged, Value: "real-token",
		})).To(Succeed())

		env, managed, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{Name: "TOKEN"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(HaveKey("TOKEN"))
		placeholder := env["TOKEN"]
		Expect(placeholder).To(HavePrefix("kvarn_"))
		Expect(placeholder).NotTo(Equal("real-token"))
		Expect(managed).To(HaveKey(placeholder))
		Expect(managed[placeholder].Value).To(Equal("real-token"))
	})

	It("carries scheme and hosts through to the managed entry", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "DOCKERHUB",
			Type: secret.TypeManaged, Value: "dh-token",
		})).To(Succeed())

		env, managed, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{
			Name: "DOCKERHUB", Scheme: "basic", Hosts: []string{"registry-1.docker.io"},
		}})
		Expect(err).NotTo(HaveOccurred())
		m := managed[env["DOCKERHUB"]]
		Expect(m.Value).To(Equal("dh-token"))
		Expect(m.Scheme).To(Equal("basic"))
		Expect(m.Hosts).To(ConsistOf("registry-1.docker.io"))
	})

	It("rejects a scheme set on an env-typed secret", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "PLAIN",
			Type: secret.TypeEnv, Value: "v",
		})).To(Succeed())

		_, _, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{Name: "PLAIN", Scheme: "bearer"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("apply only to"))
	})

	It("rejects hosts set on an env-typed secret", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "PLAIN",
			Type: secret.TypeEnv, Value: "v",
		})).To(Succeed())

		_, _, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{Name: "PLAIN", Hosts: []string{"example.com"}}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("apply only to"))
	})

	It("resolves a mix of env and managed secrets in a single call", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "HMAC_SIGN",
			Type: secret.TypeEnv, Value: "hmac",
		})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "DOCKERHUB",
			Type: secret.TypeManaged, Value: "dh-token",
		})).To(Succeed())

		env, managed, err := secret.Resolve(ctx, store, "demo",
			[]secret.Ref{{Name: "HMAC_SIGN"}, {Name: "DOCKERHUB"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(env).To(HaveKeyWithValue("HMAC_SIGN", "hmac"))
		Expect(env["DOCKERHUB"]).To(HavePrefix("kvarn_"))
		Expect(managed[env["DOCKERHUB"]].Value).To(Equal("dh-token"))
	})

	It("reports every missing name in a single error", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "PRESENT",
			Type: secret.TypeEnv, Value: "v",
		})).To(Succeed())

		_, _, err := secret.Resolve(ctx, store, "demo",
			[]secret.Ref{{Name: "PRESENT"}, {Name: "MISSING_A"}, {Name: "MISSING_B"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("missing secrets for project \"demo\""))
		Expect(err.Error()).To(ContainSubstring("MISSING_A"))
		Expect(err.Error()).To(ContainSubstring("MISSING_B"))
		Expect(err.Error()).NotTo(ContainSubstring("PRESENT"))
	})

	It("errors when names are requested but no store is configured", func() {
		_, _, err := secret.Resolve(ctx, nil, "demo", []secret.Ref{{Name: "X"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no secret store is configured"))
	})

	It("rejects secrets with an unknown type", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "WEIRD",
			Type: "aws", Value: "v",
		})).To(Succeed())

		_, _, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{Name: "WEIRD"}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unknown type"))
	})

	It("uses a fresh placeholder per managed secret", func() {
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "A",
			Type: secret.TypeManaged, Value: "value-a",
		})).To(Succeed())
		Expect(store.Put(ctx, &secret.Secret{
			Project: "demo", Name: "B",
			Type: secret.TypeManaged, Value: "value-b",
		})).To(Succeed())

		env, managed, err := secret.Resolve(ctx, store, "demo", []secret.Ref{{Name: "A"}, {Name: "B"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(env["A"]).NotTo(Equal(env["B"]))
		Expect(managed).To(HaveLen(2))
	})
})
