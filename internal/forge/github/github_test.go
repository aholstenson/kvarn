package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
		It("creates a PR with credentials and sends nothing else", func() {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/pulls"))
				Expect(r.Header.Get("Authorization")).To(Equal("Bearer test-token"))
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{
					"number":   42,
					"html_url": "https://github.com/owner/repo/pull/42",
				})
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
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pr.Ref).To(Equal("42"))
			Expect(pr.URL).To(Equal("https://github.com/owner/repo/pull/42"))
			Expect(paths).To(Equal([]string{"/repos/owner/repo/pulls"}), "labels are added by AddLabels, not here")
		})

		// sentTitle creates a PR against a stub server and returns the title
		// that reached the API, which is what the length limit applies to.
		sentTitle := func(title string) string {
			var sent string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/owner/repo/pulls" {
					var body struct {
						Title string `json:"title"`
					}
					Expect(json.NewDecoder(r.Body).Decode(&body)).To(Succeed())
					sent = body.Title
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{
						"number":   42,
						"html_url": "https://github.com/owner/repo/pull/42",
					})
				}
			}))
			defer server.Close()

			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)
			_, err := gh.CreatePullRequest(context.Background(), forge.CreatePROpts{
				RepoURL:     "https://github.com/owner/repo.git",
				BaseBranch:  "main",
				HeadBranch:  "feature",
				Title:       title,
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
			})
			Expect(err).NotTo(HaveOccurred())
			return sent
		}

		It("truncates a title the API would reject", func() {
			sent := sentTitle(strings.Repeat("a", 300))
			Expect([]rune(sent)).To(HaveLen(256))
			Expect(sent).To(HaveSuffix("…"), "the cut is marked")
		})

		It("counts characters rather than bytes", func() {
			// 200 three-byte runes are 600 bytes but fit the limit.
			title := strings.Repeat("ä", 200)
			Expect(sentTitle(title)).To(Equal(title))
		})

		It("does not cut a title that fits", func() {
			title := strings.Repeat("a", 256)
			Expect(sentTitle(title)).To(Equal(title))
		})

		It("leaves an over-budget but acceptable title as written", func() {
			title := "fix: " + strings.Repeat("word ", 20)
			Expect(sentTitle(title)).To(Equal(title), "the style budget is not enforced here")
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

	Describe("UpdatePullRequest", func() {
		var (
			server       *httptest.Server
			gh           *forgegithub.GitHub
			requests     int
			capturedBody map[string]any
			status       int
		)

		BeforeEach(func() {
			requests = 0
			capturedBody = nil
			status = http.StatusOK
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/pulls/42"))
				Expect(r.Method).To(Equal(http.MethodPatch))
				Expect(r.Header.Get("Authorization")).To(Equal("Bearer test-token"))
				Expect(json.NewDecoder(r.Body).Decode(&capturedBody)).To(Succeed())
				w.WriteHeader(status)
				w.Write([]byte(`{}`))
			}))
			DeferCleanup(server.Close)
			gh = forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)
		})

		update := func(title, body *string) error {
			return gh.UpdatePullRequest(context.Background(), forge.UpdatePROpts{
				RepoURL:     "https://github.com/owner/repo.git",
				PRRef:       "42",
				Title:       title,
				Body:        body,
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
			})
		}
		ptr := func(s string) *string { return &s }

		It("sends both fields when both are set", func() {
			Expect(update(ptr("New title"), ptr("New body"))).To(Succeed())
			Expect(capturedBody).To(Equal(map[string]any{"title": "New title", "body": "New body"}))
		})

		It("leaves out a field that is not set", func() {
			Expect(update(nil, ptr("New body"))).To(Succeed())
			Expect(capturedBody).To(Equal(map[string]any{"body": "New body"}))
		})

		It("makes no request when nothing is set", func() {
			Expect(update(nil, nil)).To(Succeed())
			Expect(requests).To(BeZero())
		})

		It("truncates a title the API would reject", func() {
			Expect(update(ptr(strings.Repeat("a", 300)), nil)).To(Succeed())
			Expect([]rune(capturedBody["title"].(string))).To(HaveLen(256))
		})

		It("returns an error on a non-200 response", func() {
			status = http.StatusUnprocessableEntity
			err := update(ptr("t"), nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("HTTP 422"))
		})
	})

	Describe("HTTP client", func() {
		It("has a timeout by default", func() {
			Expect(forgegithub.HTTPClientTimeout(forgegithub.New())).To(BeNumerically(">", 0))
		})

		It("reports a request that runs past the timeout as transient", func() {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			DeferCleanup(server.Close)
			DeferCleanup(func() { close(release) })

			client := server.Client()
			client.Timeout = 50 * time.Millisecond
			gh := forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(client),
			)
			err := gh.PostComment(context.Background(), forge.PostCommentOpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "1",
				Body:    "hi",
			})
			Expect(err).To(HaveOccurred())
			Expect(forge.IsTransient(err)).To(BeTrue())
		})
	})

	Describe("error classification", func() {
		var (
			server  *httptest.Server
			gh      *forgegithub.GitHub
			status  int
			headers map[string]string
			body    string
		)

		BeforeEach(func() {
			status = http.StatusInternalServerError
			headers = map[string]string{}
			body = `{"message":"nope"}`
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(status)
				w.Write([]byte(body))
			}))
			DeferCleanup(server.Close)
			gh = forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)
		})

		createPR := func() error {
			_, err := gh.CreatePullRequest(context.Background(), forge.CreatePROpts{
				RepoURL:    "https://github.com/owner/repo.git",
				BaseBranch: "main",
				HeadBranch: "feature",
				Title:      "t",
			})
			return err
		}

		apiError := func(err error) *forge.APIError {
			var apiErr *forge.APIError
			Expect(errors.As(err, &apiErr)).To(BeTrue(), "error %v is an APIError", err)
			return apiErr
		}

		DescribeTable("server errors are transient",
			func(code int) {
				status = code
				err := createPR()
				Expect(apiError(err).StatusCode).To(Equal(code))
				Expect(apiError(err).Op).To(Equal("create PR"))
				Expect(apiError(err).Body).To(ContainSubstring("nope"))
				Expect(forge.IsTransient(err)).To(BeTrue())
			},
			Entry("500", http.StatusInternalServerError),
			Entry("502", http.StatusBadGateway),
		)

		It("reads Retry-After in seconds on a 429", func() {
			status = http.StatusTooManyRequests
			headers["Retry-After"] = "7"
			err := createPR()
			Expect(forge.IsTransient(err)).To(BeTrue())
			Expect(apiError(err).RateLimited).To(BeTrue())
			Expect(forge.RetryAfterOf(err)).To(Equal(7 * time.Second))
		})

		It("reads Retry-After as an HTTP date", func() {
			status = http.StatusTooManyRequests
			headers["Retry-After"] = time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
			err := createPR()
			Expect(forge.RetryAfterOf(err)).To(BeNumerically("~", 30*time.Second, 2*time.Second))
		})

		It("treats a 403 with an exhausted rate limit as a transient rate limit", func() {
			status = http.StatusForbidden
			headers["X-RateLimit-Remaining"] = "0"
			headers["X-RateLimit-Reset"] = strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
			err := createPR()
			Expect(apiError(err).RateLimited).To(BeTrue())
			Expect(forge.IsTransient(err)).To(BeTrue())
			Expect(forge.RetryAfterOf(err)).To(BeNumerically("~", 90*time.Second, 2*time.Second))
		})

		It("never asks to wait a negative time for a reset in the past", func() {
			status = http.StatusForbidden
			headers["X-RateLimit-Remaining"] = "0"
			headers["X-RateLimit-Reset"] = strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
			err := createPR()
			Expect(forge.IsTransient(err)).To(BeTrue())
			Expect(forge.RetryAfterOf(err)).To(BeZero())
		})

		It("treats a plain 403 as permanent", func() {
			status = http.StatusForbidden
			headers["X-RateLimit-Remaining"] = "4999"
			err := createPR()
			Expect(apiError(err).RateLimited).To(BeFalse())
			Expect(forge.IsTransient(err)).To(BeFalse())
		})

		It("returns a 422 on create as a permanent APIError", func() {
			status = http.StatusUnprocessableEntity
			err := createPR()
			Expect(apiError(err).StatusCode).To(Equal(http.StatusUnprocessableEntity))
			Expect(forge.IsTransient(err)).To(BeFalse())
		})

		It("returns an APIError when the installation token cannot be created", func() {
			status = http.StatusServiceUnavailable
			privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
			Expect(err).NotTo(HaveOccurred())
			keyPath := filepath.Join(GinkgoT().TempDir(), "app.pem")
			Expect(os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
			}), 0600)).To(Succeed())

			_, err = gh.ResolveCredentials(context.Background(), map[string]string{
				"app_id":           "1",
				"private_key_path": keyPath,
				"installation_id":  "2",
			})
			Expect(apiError(err).Op).To(Equal("create installation token"))
			Expect(forge.IsTransient(err)).To(BeTrue())
		})

		It("treats an unreadable private key as permanent", func() {
			_, err := gh.ResolveCredentials(context.Background(), map[string]string{
				"app_id":           "1",
				"private_key_path": filepath.Join(GinkgoT().TempDir(), "missing.pem"),
				"installation_id":  "2",
			})
			Expect(err).To(HaveOccurred())
			Expect(forge.IsTransient(err)).To(BeFalse())
		})

		It("treats local input errors as permanent", func() {
			_, err := gh.CreatePullRequest(context.Background(), forge.CreatePROpts{RepoURL: "not a url"})
			Expect(err).To(HaveOccurred())
			Expect(forge.IsTransient(err)).To(BeFalse())

			err = gh.PostComment(context.Background(), forge.PostCommentOpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "x",
			})
			Expect(err).To(HaveOccurred())
			Expect(forge.IsTransient(err)).To(BeFalse())
		})

		It("treats an unreadable success body as permanent", func() {
			status = http.StatusCreated
			body = `not json`
			err := createPR()
			Expect(err).To(MatchError(ContainSubstring("parse PR response")))
			Expect(forge.IsTransient(err)).To(BeFalse())
		})
	})

	Describe("AddLabels", func() {
		var (
			server       *httptest.Server
			gh           *forgegithub.GitHub
			requests     int
			capturedBody map[string]any
		)

		BeforeEach(func() {
			requests = 0
			capturedBody = nil
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/issues/42/labels"))
				Expect(r.Method).To(Equal(http.MethodPost))
				Expect(r.Header.Get("Authorization")).To(Equal("Bearer test-token"))
				Expect(json.NewDecoder(r.Body).Decode(&capturedBody)).To(Succeed())
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`[]`))
			}))
			DeferCleanup(server.Close)
			gh = forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)
		})

		addLabels := func(labels []string) error {
			return gh.AddLabels(context.Background(), forge.AddLabelsOpts{
				RepoURL:     "https://github.com/owner/repo.git",
				PRRef:       "42",
				Labels:      labels,
				Credentials: scm.StaticCredentials(&scm.Credentials{Token: "test-token"}),
			})
		}

		It("posts the labels", func() {
			Expect(addLabels([]string{"bot", "kvarn"})).To(Succeed())
			Expect(requests).To(Equal(1))
			Expect(capturedBody).To(Equal(map[string]any{"labels": []any{"bot", "kvarn"}}))
		})

		It("makes no request when there are no labels", func() {
			Expect(addLabels(nil)).To(Succeed())
			Expect(requests).To(BeZero())
		})
	})

	Describe("FindPullRequest", func() {
		var (
			server   *httptest.Server
			gh       *forgegithub.GitHub
			query    url.Values
			response string
		)

		BeforeEach(func() {
			query = nil
			response = `[]`
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/pulls"))
				Expect(r.Method).To(Equal(http.MethodGet))
				query = r.URL.Query()
				w.Write([]byte(response))
			}))
			DeferCleanup(server.Close)
			gh = forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)
		})

		find := func(branch string) (*forge.PullRequest, error) {
			return gh.FindPullRequest(context.Background(), forge.FindPROpts{
				RepoURL:    "https://github.com/owner/repo.git",
				HeadBranch: branch,
			})
		}

		It("asks for the newest PR from the branch in the repository itself", func() {
			response = `[{"number": 7, "html_url": "https://github.com/owner/repo/pull/7"}]`
			pr, err := find("kvarn/fix & more")
			Expect(err).NotTo(HaveOccurred())
			Expect(pr).To(Equal(&forge.PullRequest{URL: "https://github.com/owner/repo/pull/7", Ref: "7"}))
			Expect(query.Get("head")).To(Equal("owner:kvarn/fix & more"))
			Expect(query.Get("state")).To(Equal("all"))
			Expect(query.Get("sort")).To(Equal("created"))
			Expect(query.Get("direction")).To(Equal("desc"))
			Expect(query.Get("per_page")).To(Equal("1"))
		})

		It("returns nil when there is no PR", func() {
			pr, err := find("kvarn/none")
			Expect(err).NotTo(HaveOccurred())
			Expect(pr).To(BeNil())
		})
	})

	Describe("HasComment", func() {
		var (
			server  *httptest.Server
			gh      *forgegithub.GitHub
			pages   map[string][]string
			queries []url.Values
		)

		BeforeEach(func() {
			pages = map[string][]string{}
			queries = nil
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Expect(r.URL.Path).To(Equal("/repos/owner/repo/issues/42/comments"))
				Expect(r.Method).To(Equal(http.MethodGet))
				queries = append(queries, r.URL.Query())
				comments := []map[string]string{}
				for _, body := range pages[r.URL.Query().Get("page")] {
					comments = append(comments, map[string]string{"body": body})
				}
				json.NewEncoder(w).Encode(comments)
			}))
			DeferCleanup(server.Close)
			gh = forgegithub.New(
				forgegithub.WithAPIBase(server.URL),
				forgegithub.WithHTTPClient(server.Client()),
			)
		})

		fullPage := func(body string) []string {
			out := make([]string, 100)
			for i := range out {
				out[i] = body
			}
			return out
		}

		hasComment := func(marker string, since time.Time) (bool, error) {
			return gh.HasComment(context.Background(), forge.HasCommentOpts{
				RepoURL: "https://github.com/owner/repo.git",
				PRRef:   "42",
				Marker:  marker,
				Since:   since,
			})
		}

		It("finds the marker on a later page", func() {
			pages["1"] = fullPage("unrelated")
			pages["2"] = []string{"other", "done <!-- kvarn:abc -->"}
			found, err := hasComment("<!-- kvarn:abc -->", time.Time{})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(queries).To(HaveLen(2))
			Expect(queries[0].Get("per_page")).To(Equal("100"))
			Expect(queries[0].Has("since")).To(BeFalse())
		})

		It("passes since in UTC", func() {
			since := time.Date(2026, 3, 4, 12, 30, 0, 0, time.FixedZone("CET", 3600))
			_, err := hasComment("m", since)
			Expect(err).NotTo(HaveOccurred())
			Expect(queries[0].Get("since")).To(Equal("2026-03-04T11:30:00Z"))
		})

		It("returns false when no comment has the marker", func() {
			pages["1"] = []string{"one", "two"}
			found, err := hasComment("<!-- kvarn:abc -->", time.Time{})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			Expect(queries).To(HaveLen(1), "a short page is the last one")
		})

		It("stops at the page limit", func() {
			for i := 1; i <= 30; i++ {
				pages[strconv.Itoa(i)] = fullPage("unrelated")
			}
			found, err := hasComment("m-absent", time.Time{})
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse())
			Expect(queries).To(HaveLen(20))
		})

		It("refuses an empty marker as permanent", func() {
			_, err := hasComment("", time.Time{})
			Expect(err).To(HaveOccurred())
			Expect(forge.IsTransient(err)).To(BeFalse())
			Expect(queries).To(BeEmpty())
		})
	})
})
