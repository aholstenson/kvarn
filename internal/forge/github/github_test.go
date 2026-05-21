package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/forge"
	forgegithub "github.com/aholstenson/kvarn/internal/forge/github"
	"github.com/aholstenson/kvarn/internal/scm"
)

var _ = Describe("GitHub Forge", func() {
	Describe("ResolveCloneURL", func() {
		var gh *forgegithub.GitHub

		BeforeEach(func() {
			gh = forgegithub.New()
		})

		It("resolves shorthand to HTTPS URL", func() {
			url, err := gh.ResolveCloneURL("myorg/myrepo")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("https://github.com/myorg/myrepo.git"))
		})

		It("passes through HTTPS URLs", func() {
			url, err := gh.ResolveCloneURL("https://github.com/myorg/myrepo.git")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("https://github.com/myorg/myrepo.git"))
		})

		It("passes through SSH URLs", func() {
			url, err := gh.ResolveCloneURL("git@github.com:myorg/myrepo.git")
			Expect(err).NotTo(HaveOccurred())
			Expect(url).To(Equal("git@github.com:myorg/myrepo.git"))
		})

		It("rejects invalid shorthand", func() {
			_, err := gh.ResolveCloneURL("just-a-name")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid repo reference"))
		})

		It("rejects empty owner", func() {
			_, err := gh.ResolveCloneURL("/repo")
			Expect(err).To(HaveOccurred())
		})

		It("rejects empty repo", func() {
			_, err := gh.ResolveCloneURL("owner/")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ResolveCredentials", func() {
		It("resolves PAT credentials", func() {
			gh := forgegithub.New()
			creds, err := gh.ResolveCredentials(context.Background(), map[string]string{
				"token": "ghp_test123",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Token).To(Equal("ghp_test123"))
		})

		It("returns error for empty config", func() {
			gh := forgegithub.New()
			_, err := gh.ResolveCredentials(context.Background(), map[string]string{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("token"))
		})

		It("resolves GitHub App credentials", func() {
			// Generate test RSA key.
			privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
			Expect(err).NotTo(HaveOccurred())

			keyPEM := pem.EncodeToMemory(&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
			})

			tmpDir, err := os.MkdirTemp("", "github-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			keyPath := filepath.Join(tmpDir, "app.pem")
			Expect(os.WriteFile(keyPath, keyPEM, 0600)).To(Succeed())

			// Mock GitHub API server.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/app/installations/67890/access_tokens"))
				Expect(r.Method).To(Equal(http.MethodPost))
				Expect(r.Header.Get("Authorization")).To(HavePrefix("Bearer "))

				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{
					"token":      "ghs_test_installation_token",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				})
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			creds, err := gh.ResolveCredentials(context.Background(), map[string]string{
				"app_id":          "12345",
				"private_key_path": keyPath,
				"installation_id": "67890",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Token).To(Equal("ghs_test_installation_token"))
		})

		It("caches installation tokens", func() {
			privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
			Expect(err).NotTo(HaveOccurred())

			keyPEM := pem.EncodeToMemory(&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
			})

			tmpDir, err := os.MkdirTemp("", "github-test-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			keyPath := filepath.Join(tmpDir, "app.pem")
			Expect(os.WriteFile(keyPath, keyPEM, 0600)).To(Succeed())

			callCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callCount++
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{
					"token":      "ghs_cached_token",
					"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
				})
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			config := map[string]string{
				"app_id":          "12345",
				"private_key_path": keyPath,
				"installation_id": "99999",
			}

			// First call hits the server.
			creds1, err := gh.ResolveCredentials(context.Background(), config)
			Expect(err).NotTo(HaveOccurred())
			Expect(creds1.Token).To(Equal("ghs_cached_token"))
			Expect(callCount).To(Equal(1))

			// Second call uses cache.
			creds2, err := gh.ResolveCredentials(context.Background(), config)
			Expect(err).NotTo(HaveOccurred())
			Expect(creds2.Token).To(Equal("ghs_cached_token"))
			Expect(callCount).To(Equal(1))
		})
	})

	Describe("CreatePullRequest", func() {
		It("creates a PR with credentials", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/repos/owner/repo/pulls":
					Expect(r.Header.Get("Authorization")).To(Equal("Bearer test-token"))
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{
						"number":   42,
						"html_url": "https://github.com/owner/repo/pull/42",
					})
				case r.URL.Path == "/repos/owner/repo/issues/42/labels":
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode([]map[string]string{})
				}
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			pr, err := gh.CreatePullRequest(context.Background(), forge.CreatePROpts{
				RepoURL:     "https://github.com/owner/repo.git",
				BaseBranch:  "main",
				HeadBranch:  "feature",
				Title:       "Test PR",
				Body:        "Test body",
				Labels:      []string{"bot"},
				Credentials: &scm.Credentials{Token: "test-token"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Number).To(Equal(42))
			Expect(pr.URL).To(Equal("https://github.com/owner/repo/pull/42"))
		})
	})

	Describe("PostComment", func() {
		It("posts a comment on an existing PR", func() {
			var capturedBody map[string]any
			var capturedAuth string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/issues/42/comments"))
				Expect(r.Method).To(Equal(http.MethodPost))
				capturedAuth = r.Header.Get("Authorization")
				Expect(json.NewDecoder(r.Body).Decode(&capturedBody)).To(Succeed())
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{"id": 1})
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			err := gh.PostComment(context.Background(), forge.PostCommentOpts{
				RepoURL:     "https://github.com/owner/repo.git",
				Number:      42,
				Body:        "Hello from kvarn",
				Credentials: &scm.Credentials{Token: "test-token"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(capturedAuth).To(Equal("Bearer test-token"))
			Expect(capturedBody).To(HaveKeyWithValue("body", "Hello from kvarn"))
		})

		It("returns an error on non-201 response", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"message":"Bad credentials"}`))
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			err := gh.PostComment(context.Background(), forge.PostCommentOpts{
				RepoURL:     "https://github.com/owner/repo.git",
				Number:      7,
				Body:        "hi",
				Credentials: &scm.Credentials{Token: "bad-token"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HTTP 401"))
		})
	})
})
