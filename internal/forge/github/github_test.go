package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
			src, err := gh.ResolveCredentials(context.Background(), map[string]string{
				"token": "ghp_test123",
			})
			Expect(err).NotTo(HaveOccurred())
			creds, err := src.Credentials(context.Background())
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

			src, err := gh.ResolveCredentials(context.Background(), map[string]string{
				"app_id":           "12345",
				"private_key_path": keyPath,
				"installation_id":  "67890",
			})
			Expect(err).NotTo(HaveOccurred())
			creds, err := src.Credentials(context.Background())
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
				"app_id":           "12345",
				"private_key_path": keyPath,
				"installation_id":  "99999",
			}

			// Resolving the source mints the first token.
			src, err := gh.ResolveCredentials(context.Background(), config)
			Expect(err).NotTo(HaveOccurred())
			Expect(callCount).To(Equal(1))

			// Re-reading the source while the token is still valid is free.
			creds1, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(creds1.Token).To(Equal("ghs_cached_token"))
			Expect(callCount).To(Equal(1))

			creds2, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(creds2.Token).To(Equal("ghs_cached_token"))
			Expect(callCount).To(Equal(1))
		})

		It("re-mints an installation token that has expired", func() {
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

			// Hand out already-expired tokens so the source cannot serve the
			// cache: this is the state of a job that outran its credentials.
			callCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callCount++
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{
					"token":      fmt.Sprintf("ghs_token_%d", callCount),
					"expires_at": time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
				})
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			src, err := gh.ResolveCredentials(context.Background(), map[string]string{
				"app_id":           "12345",
				"private_key_path": keyPath,
				"installation_id":  "55555",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(callCount).To(Equal(1))

			creds, err := src.Credentials(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(creds.Token).To(Equal("ghs_token_2"))
			Expect(callCount).To(Equal(2))
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
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Ref).To(Equal("42"))
			Expect(pr.URL).To(Equal("https://github.com/owner/repo/pull/42"))
		})
	})

	Describe("GetPullRequest", func() {
		// prPayload builds a pulls/{n} response body with the given head and
		// base repositories, so tests can vary just the fork-relevant fields.
		prPayload := func(headRepo, baseRepo string, merged bool) map[string]any {
			return map[string]any{
				"number":   42,
				"state":    "open",
				"merged":   merged,
				"title":    "Add a helper",
				"body":     "Adds a small helper.",
				"html_url": "https://github.com/owner/repo/pull/42",
				"head": map[string]any{
					"ref":  "kvarn/add-a-helper",
					"sha":  "abc123",
					"repo": map[string]any{"full_name": headRepo},
				},
				"base": map[string]any{
					"ref":  "main",
					"repo": map[string]any{"full_name": baseRepo},
				},
			}
		}

		It("reads an open PR on the same repository", func() {
			var capturedAccept string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/pulls/42"))
				Expect(r.Method).To(Equal(http.MethodGet))
				capturedAccept = r.Header.Get("Accept")
				json.NewEncoder(w).Encode(prPayload("owner/repo", "owner/repo", false))
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			pr, err := gh.GetPullRequest(context.Background(), forge.GetPROpts{
				RepoURL:     "https://github.com/owner/repo.git",
				PRRef:       "42",
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(capturedAccept).To(Equal("application/vnd.github+json"))
			Expect(pr.Ref).To(Equal("42"))
			Expect(pr.State).To(Equal("open"))
			Expect(pr.HeadBranch).To(Equal("kvarn/add-a-helper"))
			Expect(pr.HeadSHA).To(Equal("abc123"))
			Expect(pr.HeadRepo).To(Equal("owner/repo"))
			Expect(pr.BaseBranch).To(Equal("main"))
			Expect(pr.BaseRepo).To(Equal("owner/repo"))
			Expect(pr.Title).To(Equal("Add a helper"))
			Expect(pr.URL).To(Equal("https://github.com/owner/repo/pull/42"))
		})

		It("reports differing head and base repositories for a fork PR", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				json.NewEncoder(w).Encode(prPayload("contributor/repo", "owner/repo", false))
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			pr, err := gh.GetPullRequest(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "42",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.HeadRepo).To(Equal("contributor/repo"))
			Expect(pr.BaseRepo).To(Equal("owner/repo"))
		})

		It("reports a merged PR as merged rather than closed", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				payload := prPayload("owner/repo", "owner/repo", true)
				payload["state"] = "closed"
				json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			pr, err := gh.GetPullRequest(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "42",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.State).To(Equal("merged"))
		})

		It("returns a clear error for a non-numeric ref", func() {
			gh := forgegithub.New()
			_, err := gh.GetPullRequest(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "not-a-number",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid GitHub pull request ref"))
		})

		It("returns an error on a non-200 response", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"Not Found"}`))
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			_, err := gh.GetPullRequest(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "99",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HTTP 404"))
		})
	})

	Describe("GetPullRequestDiff", func() {
		It("requests the diff media type and returns the body", func() {
			var capturedAccept string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/pulls/42"))
				capturedAccept = r.Header.Get("Accept")
				w.Write([]byte("diff --git a/x b/x\n+hello\n"))
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			diff, err := gh.GetPullRequestDiff(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "42",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(capturedAccept).To(Equal("application/vnd.github.v3.diff"))
			Expect(diff).To(Equal("diff --git a/x b/x\n+hello\n"))
		})

		It("truncates an oversized diff with a marker", func() {
			huge := strings.Repeat("+line\n", 100000) // well past the 256 KiB cap
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(huge))
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)

			diff, err := gh.GetPullRequestDiff(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "42",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(diff)).To(BeNumerically("<", len(huge)))
			Expect(diff).To(HaveSuffix("[diff truncated]\n"))
		})

		It("returns a clear error for a non-numeric ref", func() {
			gh := forgegithub.New()
			_, err := gh.GetPullRequestDiff(context.Background(), forge.GetPROpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "abc",
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("invalid GitHub pull request ref"))
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
				PRRef:       "42",
				Body:        "Hello from kvarn",
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
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
				PRRef:       "7",
				Body:        "hi",
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "bad-token"}),
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HTTP 401"))
		})
	})
})
