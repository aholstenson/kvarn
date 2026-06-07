package proxy_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/egress/proxy"
)

var _ = Describe("PlaceholderInjector", func() {
	const (
		realToken   = "ghp_realsecret_value"
		placeholder = "kvarn_0123456789abcdef0123456789abcdef"
	)

	Describe("bearer scheme", func() {
		var injector *proxy.PlaceholderInjector

		BeforeEach(func() {
			injector = proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken, Scheme: proxy.SchemeBearer},
			}, nil)
		})

		It("substitutes placeholder in Authorization header", func() {
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Set("Authorization", "Bearer "+placeholder)

			Expect(injector.Inject(req, "example.com")).To(Succeed())
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + realToken))
		})

		It("substitutes placeholder in arbitrary headers", func() {
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Set("X-Custom-Auth", placeholder)
			req.Header.Set("X-Another", "prefix-"+placeholder+"-suffix")

			Expect(injector.Inject(req, "example.com")).To(Succeed())
			Expect(req.Header.Get("X-Custom-Auth")).To(Equal(realToken))
			Expect(req.Header.Get("X-Another")).To(Equal("prefix-" + realToken + "-suffix"))
		})

		It("substitutes across all values of a multi-value header", func() {
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Add("X-Multi", "a")
			req.Header.Add("X-Multi", placeholder)
			req.Header.Add("X-Multi", "z")

			Expect(injector.Inject(req, "example.com")).To(Succeed())
			Expect(req.Header.Values("X-Multi")).To(Equal([]string{"a", realToken, "z"}))
		})

		It("leaves the body untouched even when it contains the placeholder", func() {
			body := "payload-includes-" + placeholder
			req, _ := http.NewRequest("POST", "https://example.com/", io.NopCloser(bytes.NewBufferString(body)))

			Expect(injector.Inject(req, "example.com")).To(Succeed())

			read, _ := io.ReadAll(req.Body)
			Expect(string(read)).To(Equal(body))
		})

		It("is a no-op when no placeholder matches", func() {
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Set("Authorization", "Bearer not-a-placeholder")

			Expect(injector.Inject(req, "example.com")).To(Succeed())
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer not-a-placeholder"))
		})

		It("defaults an empty scheme to bearer", func() {
			inj := proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken},
			}, nil)
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Set("Authorization", "Bearer "+placeholder)

			Expect(inj.Inject(req, "example.com")).To(Succeed())
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + realToken))
		})

		It("is a no-op when constructed with an empty map", func() {
			empty := proxy.NewPlaceholderInjector(nil, nil)
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Set("Authorization", "Bearer "+placeholder)

			Expect(empty.Inject(req, "example.com")).To(Succeed())
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + placeholder))
		})
	})

	Describe("basic scheme", func() {
		var injector *proxy.PlaceholderInjector

		BeforeEach(func() {
			injector = proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken, Scheme: proxy.SchemeBasic},
			}, nil)
		})

		It("decodes, substitutes, and re-encodes a Basic auth blob", func() {
			blob := base64.StdEncoding.EncodeToString([]byte("user:" + placeholder))
			req, _ := http.NewRequest("GET", "https://registry-1.docker.io/", nil)
			req.Header.Set("Authorization", "Basic "+blob)

			Expect(injector.Inject(req, "registry-1.docker.io")).To(Succeed())

			got := req.Header.Get("Authorization")
			Expect(got).To(HavePrefix("Basic "))
			decoded, err := base64.StdEncoding.DecodeString(got[len("Basic "):])
			Expect(err).NotTo(HaveOccurred())
			Expect(string(decoded)).To(Equal("user:" + realToken))
		})

		It("does not touch a plaintext (non-Basic) header value", func() {
			req, _ := http.NewRequest("GET", "https://registry-1.docker.io/", nil)
			req.Header.Set("Authorization", "Bearer "+placeholder)

			Expect(injector.Inject(req, "registry-1.docker.io")).To(Succeed())
			// A basic-scheme secret never substitutes a raw header occurrence.
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + placeholder))
		})
	})

	Describe("scheme isolation", func() {
		It("does not decode a bearer secret out of a Basic blob", func() {
			injector := proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken, Scheme: proxy.SchemeBearer},
			}, nil)
			blob := base64.StdEncoding.EncodeToString([]byte("user:" + placeholder))
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			req.Header.Set("Authorization", "Basic "+blob)

			Expect(injector.Inject(req, "example.com")).To(Succeed())
			// Bearer only does literal header replacement; the placeholder is
			// hidden inside the base64 blob, so nothing changes.
			Expect(req.Header.Get("Authorization")).To(Equal("Basic " + blob))
		})
	})

	Describe("oauth scheme", func() {
		var injector *proxy.PlaceholderInjector

		BeforeEach(func() {
			injector = proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken, Scheme: proxy.SchemeOAuth},
			}, nil)
		})

		It("rewrites a form-encoded body and updates the length", func() {
			body := "grant_type=client_credentials&client_secret=" + placeholder
			req, _ := http.NewRequest("POST", "https://auth.example.com/token", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			Expect(injector.Inject(req, "auth.example.com")).To(Succeed())

			read, _ := io.ReadAll(req.Body)
			want := "grant_type=client_credentials&client_secret=" + realToken
			Expect(string(read)).To(Equal(want))
			Expect(req.ContentLength).To(Equal(int64(len(want))))
			Expect(req.Header.Get("Content-Length")).To(Equal(strconv.Itoa(len(want))))
		})

		It("rewrites a JSON body", func() {
			body := `{"client_secret":"` + placeholder + `"}`
			req, _ := http.NewRequest("POST", "https://auth.example.com/token", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")

			Expect(injector.Inject(req, "auth.example.com")).To(Succeed())

			read, _ := io.ReadAll(req.Body)
			Expect(string(read)).To(Equal(`{"client_secret":"` + realToken + `"}`))
		})

		It("does not buffer or rewrite the body for a bearer secret", func() {
			bearer := proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken, Scheme: proxy.SchemeBearer},
			}, nil)
			body := "client_secret=" + placeholder
			req, _ := http.NewRequest("POST", "https://auth.example.com/token", bytes.NewBufferString(body))

			Expect(bearer.Inject(req, "auth.example.com")).To(Succeed())

			read, _ := io.ReadAll(req.Body)
			Expect(string(read)).To(Equal(body))
		})
	})

	Describe("host scoping", func() {
		var injector *proxy.PlaceholderInjector

		BeforeEach(func() {
			injector = proxy.NewPlaceholderInjector(map[string]proxy.ManagedSecret{
				placeholder: {Value: realToken, Scheme: proxy.SchemeBearer, Hosts: []string{"registry-1.docker.io"}},
			}, nil)
		})

		It("substitutes for a matching host", func() {
			req, _ := http.NewRequest("GET", "https://registry-1.docker.io/", nil)
			req.Header.Set("Authorization", "Bearer "+placeholder)

			Expect(injector.Inject(req, "registry-1.docker.io")).To(Succeed())
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + realToken))
		})

		It("skips a non-matching host", func() {
			req, _ := http.NewRequest("GET", "https://github.com/", nil)
			req.Header.Set("Authorization", "Bearer "+placeholder)

			Expect(injector.Inject(req, "github.com")).To(Succeed())
			Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + placeholder))
		})
	})
})
