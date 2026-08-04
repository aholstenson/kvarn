package llmauth_test

import (
	"context"
	"errors"

	llms "github.com/aholstenson/llms-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/config/credential"
	"github.com/aholstenson/kvarn/internal/llmauth"
)

// fakeStore is an in-memory credential.LLMStore.
type fakeStore struct {
	creds []*credential.LLMCredential
	err   error
}

func (s *fakeStore) Get(context.Context, string) (*credential.LLMCredential, error) {
	panic("not used by Source")
}

func (s *fakeStore) List(context.Context) ([]*credential.LLMCredential, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.creds, nil
}

func (s *fakeStore) Put(context.Context, *credential.LLMCredential) error { return nil }
func (s *fakeStore) Delete(context.Context, string) error                 { return nil }

// staticFallback stands in for the environment.
type staticFallback struct {
	key    string
	called int
}

func (f *staticFallback) Credential(context.Context, string) (llms.Credential, error) {
	f.called++
	if f.key == "" {
		return llms.Credential{}, llms.ErrNoCredentials
	}
	return llms.Credential{APIKey: f.key}, nil
}

var _ = Describe("Source", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("serves a stored API key", func() {
		store := &fakeStore{creds: []*credential.LLMCredential{
			{Provider: "anthropic", APIKey: "sk-ant-stored"},
		}}
		src := llmauth.NewSource(store, llmauth.WithFallback(&staticFallback{key: "sk-ant-env"}))

		cred, err := src.Credential(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-ant-stored"))
	})

	It("prefers the store over the fallback", func() {
		fallback := &staticFallback{key: "sk-ant-env"}
		store := &fakeStore{creds: []*credential.LLMCredential{
			{Provider: "anthropic", APIKey: "sk-ant-stored"},
		}}
		src := llmauth.NewSource(store, llmauth.WithFallback(fallback))

		cred, err := src.Credential(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-ant-stored"))
		Expect(fallback.called).To(BeZero())
	})

	It("falls back for a provider the store does not cover", func() {
		fallback := &staticFallback{key: "sk-openai-env"}
		store := &fakeStore{creds: []*credential.LLMCredential{
			{Provider: "anthropic", APIKey: "sk-ant-stored"},
		}}
		src := llmauth.NewSource(store, llmauth.WithFallback(fallback))

		cred, err := src.Credential(ctx, "openai")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-openai-env"))
		Expect(fallback.called).To(Equal(1))
	})

	It("propagates the fallback's missing-credential error", func() {
		src := llmauth.NewSource(&fakeStore{}, llmauth.WithFallback(&staticFallback{}))

		_, err := src.Credential(ctx, "openai")
		Expect(err).To(MatchError(llms.ErrNoCredentials))
	})

	It("carries extra headers through", func() {
		store := &fakeStore{creds: []*credential.LLMCredential{
			{Provider: "openrouter", APIKey: "sk-or", Headers: map[string]string{"x-title": "kvarn"}},
		}}
		src := llmauth.NewSource(store, llmauth.WithFallback(&staticFallback{}))

		cred, err := src.Credential(ctx, "openrouter")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-or"))
		Expect(cred.Headers.Get("x-title")).To(Equal("kvarn"))
	})

	It("treats an entry with neither key nor headers as absent", func() {
		fallback := &staticFallback{key: "sk-ant-env"}
		store := &fakeStore{creds: []*credential.LLMCredential{{Provider: "anthropic"}}}
		src := llmauth.NewSource(store, llmauth.WithFallback(fallback))

		cred, err := src.Credential(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("sk-ant-env"))
	})

	// Editing the file is the rotation channel: the store is re-read per
	// request, so a new key takes effect without restarting the orchestrator.
	It("picks up a rotated key on the next lookup", func() {
		store := &fakeStore{creds: []*credential.LLMCredential{
			{Provider: "anthropic", APIKey: "first"},
		}}
		src := llmauth.NewSource(store, llmauth.WithFallback(&staticFallback{}))

		cred, err := src.Credential(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("first"))

		store.creds = []*credential.LLMCredential{{Provider: "anthropic", APIKey: "second"}}

		cred, err = src.Credential(ctx, "anthropic")
		Expect(err).NotTo(HaveOccurred())
		Expect(cred.APIKey).To(Equal("second"))
	})

	// A malformed file must not silently demote every provider to whatever
	// happens to be in the environment.
	It("surfaces a store read failure instead of falling back", func() {
		fallback := &staticFallback{key: "sk-ant-env"}
		store := &fakeStore{err: errors.New("parse credentials.toml: bad")}
		src := llmauth.NewSource(store, llmauth.WithFallback(fallback))

		_, err := src.Credential(ctx, "anthropic")
		Expect(err).To(MatchError(ContainSubstring("bad")))
		Expect(fallback.called).To(BeZero())
	})
})
