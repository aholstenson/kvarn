package proxy_test

import (
	"bytes"
	"io"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/egress/proxy"
)

var _ = Describe("PlaceholderInjector", func() {
	var (
		realToken   = "ghp_realsecret_value"
		placeholder = "kvarn:0123456789abcdef0123456789abcdef"
		injector    *proxy.PlaceholderInjector
	)

	BeforeEach(func() {
		injector = proxy.NewPlaceholderInjector(map[string]string{
			placeholder: realToken,
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

	It("is a no-op when constructed with an empty map", func() {
		empty := proxy.NewPlaceholderInjector(nil, nil)
		req, _ := http.NewRequest("GET", "https://example.com/", nil)
		req.Header.Set("Authorization", "Bearer "+placeholder)

		Expect(empty.Inject(req, "example.com")).To(Succeed())
		Expect(req.Header.Get("Authorization")).To(Equal("Bearer " + placeholder))
	})
})
