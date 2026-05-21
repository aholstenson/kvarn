package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/cockroachdb/errors"
	"github.com/golang-jwt/jwt/v5"
)

// GitHub implements the forge.Forge interface for GitHub.
type GitHub struct {
	apiBase    string // defaults to "https://api.github.com"
	httpClient *http.Client

	mu         sync.Mutex
	tokenCache map[string]*cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// Option configures a GitHub forge instance.
type Option func(*GitHub)

// WithAPIBase overrides the GitHub API base URL (for testing).
func WithAPIBase(base string) Option {
	return func(g *GitHub) { g.apiBase = base }
}

// WithHTTPClient overrides the HTTP client (for testing).
func WithHTTPClient(client *http.Client) Option {
	return func(g *GitHub) { g.httpClient = client }
}

// New creates a new GitHub forge.
func New(opts ...Option) *GitHub {
	g := &GitHub{
		apiBase:    "https://api.github.com",
		httpClient: http.DefaultClient,
		tokenCache: make(map[string]*cachedToken),
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func (g *GitHub) SCM() scm.SCM {
	return &git.Git{}
}

func (g *GitHub) ResolveCloneURL(repo string) (string, error) {
	// Already a full URL? Pass through.
	if strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "git@") {
		return repo, nil
	}

	// Shorthand "org/repo" -> full HTTPS URL.
	parts := strings.SplitN(repo, "/", 3)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", errors.Newf("invalid repo reference %q, expected \"owner/repo\"", repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", parts[0], parts[1]), nil
}

func (g *GitHub) ResolveCredentials(ctx context.Context, config map[string]string) (*scm.Credentials, error) {
	// PAT: config has "token".
	if token := config["token"]; token != "" {
		return &scm.Credentials{Token: token}, nil
	}

	// GitHub App: config has "app_id" + "private_key_path" + "installation_id".
	appID := config["app_id"]
	privateKeyPath := config["private_key_path"]
	installationID := config["installation_id"]
	if appID != "" && privateKeyPath != "" && installationID != "" {
		token, err := g.getInstallationToken(ctx, appID, privateKeyPath, installationID)
		if err != nil {
			return nil, errors.Wrap(err, "resolve GitHub App credentials")
		}
		return &scm.Credentials{Token: token}, nil
	}

	return nil, errors.New("github credentials require either \"token\" or \"app_id\"+\"private_key_path\"+\"installation_id\"")
}

func (g *GitHub) getInstallationToken(ctx context.Context, appID, privateKeyPath, installationID string) (string, error) {
	// Check cache.
	g.mu.Lock()
	if cached, ok := g.tokenCache[installationID]; ok {
		if time.Now().Before(cached.expiresAt) {
			token := cached.token
			g.mu.Unlock()
			return token, nil
		}
	}
	g.mu.Unlock()

	// Read and parse private key.
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", errors.Wrapf(err, "read private key %q", privateKeyPath)
	}

	key, err := jwt.ParseRSAPrivateKeyFromPEM(keyData)
	if err != nil {
		return "", errors.Wrap(err, "parse RSA private key")
	}

	// Create JWT.
	now := time.Now()
	jwtToken, err := g.signJWT(appID, key, now)
	if err != nil {
		return "", err
	}

	// Request installation access token.
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", g.apiBase, installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", errors.Wrap(err, "create token request")
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "request installation token")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", errors.Newf("create installation token: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", errors.Wrap(err, "parse token response")
	}

	// Cache with 5-minute safety margin.
	g.mu.Lock()
	g.tokenCache[installationID] = &cachedToken{
		token:     tokenResp.Token,
		expiresAt: tokenResp.ExpiresAt.Add(-5 * time.Minute),
	}
	g.mu.Unlock()

	return tokenResp.Token, nil
}

func (g *GitHub) signJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(key)
	if err != nil {
		return "", errors.Wrap(err, "sign JWT")
	}
	return signed, nil
}

func (g *GitHub) CreatePullRequest(ctx context.Context, opts forge.CreatePROpts) (*forge.PullRequest, error) {
	owner, repo, err := ParseRepoURL(opts.RepoURL)
	if err != nil {
		return nil, err
	}

	token := ""
	if opts.Credentials != nil {
		token = opts.Credentials.APIToken()
	}

	// Create pull request.
	prBody := map[string]any{
		"title": opts.Title,
		"body":  opts.Body,
		"head":  opts.HeadBranch,
		"base":  opts.BaseBranch,
	}
	bodyJSON, err := json.Marshal(prBody)
	if err != nil {
		return nil, errors.Wrap(err, "marshal PR body")
	}

	url := fmt.Sprintf("%s/repos/%s/%s/pulls", g.apiBase, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, errors.Wrap(err, "create PR request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "send PR request")
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, errors.Newf("create PR: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var prResp struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return nil, errors.Wrap(err, "parse PR response")
	}

	slog.Info("pull request created",
		"url", prResp.HTMLURL,
		"number", prResp.Number,
	)

	// Add labels if specified.
	if len(opts.Labels) > 0 {
		if err := g.addLabels(ctx, owner, repo, prResp.Number, opts.Labels, token); err != nil {
			slog.Warn("failed to add labels to PR", "error", err)
		}
	}

	return &forge.PullRequest{
		URL:    prResp.HTMLURL,
		Number: prResp.Number,
	}, nil
}

func (g *GitHub) PostComment(ctx context.Context, opts forge.PostCommentOpts) error {
	owner, repo, err := ParseRepoURL(opts.RepoURL)
	if err != nil {
		return err
	}

	token := ""
	if opts.Credentials != nil {
		token = opts.Credentials.APIToken()
	}

	body := map[string]any{"body": opts.Body}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return errors.Wrap(err, "marshal comment body")
	}

	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", g.apiBase, owner, repo, opts.Number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return errors.Wrap(err, "create comment request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "send comment request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return errors.Newf("post comment: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (g *GitHub) addLabels(ctx context.Context, owner, repo string, number int, labels []string, token string) error {
	body := map[string]any{"labels": labels}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", g.apiBase, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return errors.Newf("add labels: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// ParseRepoURL extracts the owner and repo name from a GitHub URL.
// Supports HTTPS (https://github.com/owner/repo) and SSH (git@github.com:owner/repo) formats.
func ParseRepoURL(repoURL string) (owner, repo string, err error) {
	// SSH format: git@github.com:owner/repo.git
	if strings.HasPrefix(repoURL, "git@") {
		parts := strings.SplitN(repoURL, ":", 2)
		if len(parts) != 2 {
			return "", "", errors.Newf("invalid SSH URL: %s", repoURL)
		}
		path := strings.TrimSuffix(parts[1], ".git")
		return splitOwnerRepo(path, repoURL)
	}

	// HTTPS format: https://github.com/owner/repo.git
	// Strip protocol and host.
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(repoURL, prefix) {
			path := strings.TrimPrefix(repoURL, prefix)
			// Remove host part.
			slashIdx := strings.Index(path, "/")
			if slashIdx < 0 {
				return "", "", errors.Newf("invalid HTTPS URL: %s", repoURL)
			}
			path = path[slashIdx+1:]
			path = strings.TrimSuffix(path, ".git")
			return splitOwnerRepo(path, repoURL)
		}
	}

	return "", "", errors.Newf("unsupported URL format: %s", repoURL)
}

func splitOwnerRepo(path, originalURL string) (string, string, error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.Newf("cannot extract owner/repo from URL: %s", originalURL)
	}
	return parts[0], parts[1], nil
}
