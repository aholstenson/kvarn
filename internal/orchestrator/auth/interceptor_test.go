package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/aholstenson/kvarn/internal/config/apikey"
	"github.com/aholstenson/kvarn/internal/config/tomlstore"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// memStore is an in-memory apikey.Store for interceptor tests.
type memStore struct {
	keys   map[string]*apikey.APIKey
	getErr error
}

func (m *memStore) Get(_ context.Context, keyID string) (*apikey.APIKey, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	k, ok := m.keys[keyID]
	if !ok {
		return nil, tomlstore.ErrNotFound
	}
	return k, nil
}

func (m *memStore) List(context.Context) ([]*apikey.APIKey, error) { return nil, nil }
func (m *memStore) Put(context.Context, *apikey.APIKey) error      { return nil }
func (m *memStore) Delete(context.Context, string) error           { return nil }

// header builds an Authorization header carrying the given bearer value.
func header(value string) http.Header {
	h := http.Header{}
	if value != "" {
		h.Set("Authorization", value)
	}
	return h
}

var _ = Describe("Interceptor.authenticate", func() {
	var (
		store  *memStore
		token  string
		keyID  string
		hash   string
		secret string
	)

	BeforeEach(func() {
		var err error
		token, keyID, hash, err = apikey.GenerateToken()
		Expect(err).NotTo(HaveOccurred())
		_, secret, _ = apikey.ParseToken(token)

		store = &memStore{keys: map[string]*apikey.APIKey{
			keyID: {
				KeyID:    keyID,
				Name:     "ci",
				Hash:     hash,
				Projects: []string{"proj-a"},
				Created:  time.Now().UTC(),
			},
		}}
	})

	It("accepts a valid token and returns the identity", func() {
		id, parsedKeyID, _, err := NewInterceptor(store).authenticate(header("Bearer " + token))
		Expect(err).NotTo(HaveOccurred())
		Expect(id.KeyName).To(Equal("ci"))
		Expect(id.KeyID).To(Equal(keyID))
		Expect(parsedKeyID).To(Equal(keyID))
		Expect(id.Projects).To(Equal([]string{"proj-a"}))
	})

	It("rejects a missing Authorization header as Unauthenticated", func() {
		_, _, _, err := NewInterceptor(store).authenticate(header(""))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("rejects a non-bearer Authorization header", func() {
		_, _, _, err := NewInterceptor(store).authenticate(header("Basic " + token))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("rejects a malformed token", func() {
		_, _, _, err := NewInterceptor(store).authenticate(header("Bearer not-a-real-token"))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("rejects an unknown key ID", func() {
		_, _, _, err := NewInterceptor(store).authenticate(header("Bearer kvarn_unknownid_" + secret))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("rejects a wrong secret", func() {
		_, _, _, err := NewInterceptor(store).authenticate(header("Bearer kvarn_" + keyID + "_wrongsecret"))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("rejects a disabled key", func() {
		store.keys[keyID].Disabled = true
		_, _, _, err := NewInterceptor(store).authenticate(header("Bearer " + token))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("rejects an expired key", func() {
		past := time.Now().Add(-time.Hour)
		store.keys[keyID].Expires = &past
		_, _, _, err := NewInterceptor(store).authenticate(header("Bearer " + token))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
	})

	It("returns an identical opaque error for every rejection path", func() {
		disabledToken, _, disabledHash, err := apikey.GenerateToken()
		Expect(err).NotTo(HaveOccurred())
		expiredToken, _, expiredHash, err := apikey.GenerateToken()
		Expect(err).NotTo(HaveOccurred())
		past := time.Now().Add(-time.Hour)
		store.keys["disabledid"] = &apikey.APIKey{KeyID: "disabledid", Hash: disabledHash, Disabled: true}
		store.keys["expiredid"] = &apikey.APIKey{KeyID: "expiredid", Hash: expiredHash, Expires: &past}
		_, disabledSecret, _ := apikey.ParseToken(disabledToken)
		_, expiredSecret, _ := apikey.ParseToken(expiredToken)

		headers := []http.Header{
			header(""),
			header("Basic " + token),
			header("Bearer not-a-real-token"),
			header("Bearer kvarn_unknownid_" + secret),
			header("Bearer kvarn_" + keyID + "_wrongsecret"),
			header("Bearer kvarn_disabledid_" + disabledSecret),
			header("Bearer kvarn_expiredid_" + expiredSecret),
		}
		interceptor := NewInterceptor(store)
		var messages []string
		for _, h := range headers {
			_, _, _, err := interceptor.authenticate(h)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnauthenticated))
			messages = append(messages, err.Error())
		}
		for _, m := range messages[1:] {
			Expect(m).To(Equal(messages[0]), "all rejection messages must be identical to avoid leaking which check failed")
		}
	})

	It("fails closed with Unavailable on a store error", func() {
		store.getErr = errors.New("disk on fire")
		_, _, _, err := NewInterceptor(store).authenticate(header("Bearer " + token))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
	})
})
