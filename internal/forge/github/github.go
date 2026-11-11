package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/scm"
	"github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/golang-jwt/jwt/v5"
)

// defaultHTTPTimeout bounds one request to the GitHub API, from dial to the
// last byte of the body. Without it, a connection that stops sending holds
// a delivery forever, and a retry never gets its turn.
const defaultHTTPTimeout = 60 * time.Second

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
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
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
		return "", forge.Permanent(fmt.Errorf("invalid repo reference %q, expected \"owner/repo\"", repo))
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", parts[0], parts[1]), nil
}

func (g *GitHub) ResolveCredentials(ctx context.Context, config map[string]string) (scm.CredentialSource, error) {
	// PAT: config has "token".
	if token := config["token"]; token != "" {
		return scm.StaticCredentials(&scm.Credentials{Token: token}), nil
	}

	// GitHub App: config has "app_id" + "private_key_path" + "installation_id".
	appID := config["app_id"]
	privateKeyPath := config["private_key_path"]
	installationID := config["installation_id"]
	if appID != "" && privateKeyPath != "" && installationID != "" {
		src := &installationTokenSource{
			gh:             g,
			appID:          appID,
			privateKeyPath: privateKeyPath,
			installationID: installationID,
		}
		// Mint one token now so a misconfigured app (unreadable key, wrong
		// installation) is reported where the credential is configured rather
		// than at push time. It also warms the cache for the clone that follows.
		if _, err := src.Credentials(ctx); err != nil {
			return nil, err
		}
		return src, nil
	}

	return nil, forge.Permanent(errors.New("github credentials require either \"token\" or \"app_id\"+\"private_key_path\"+\"installation_id\""))
}

// installationTokenSource mints GitHub App installation tokens on demand.
// Those tokens expire after an hour, so a job that runs longer than that must
// not hold on to the one it started with; every call goes through the cache,
// which re-mints once the current token nears expiry.
type installationTokenSource struct {
	gh             *GitHub
	appID          string
	privateKeyPath string
	installationID string
}

func (s *installationTokenSource) Credentials(ctx context.Context) (*scm.Credentials, error) {
	token, err := s.gh.getInstallationToken(ctx, s.appID, s.privateKeyPath, s.installationID)
	if err != nil {
		return nil, fmt.Errorf("resolve GitHub App credentials: %w", err)
	}
	return &scm.Credentials{Token: token}, nil
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

	// A key that cannot be read or parsed is a configuration problem, and
	// reading it again does not fix it. The read error is formatted, not
	// wrapped: its syscall.Errno satisfies net.Error, which forge.IsTransient
	// would take for a network failure.
	keyData, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", forge.Permanent(fmt.Errorf("read private key %q: %v", privateKeyPath, err))
	}

	key, err := jwt.ParseRSAPrivateKeyFromPEM(keyData)
	if err != nil {
		return "", forge.Permanent(fmt.Errorf("parse RSA private key: %w", err))
	}

	// Create JWT.
	now := time.Now()
	jwtToken, err := g.signJWT(appID, key, now)
	if err != nil {
		return "", forge.Permanent(err)
	}

	// Request installation access token.
	body, err := g.do(ctx, apiRequest{
		op:     "create installation token",
		method: http.MethodPost,
		url:    fmt.Sprintf("%s/app/installations/%s/access_tokens", g.apiBase, url.PathEscape(installationID)),
		token:  jwtToken,
		want:   http.StatusCreated,
	})
	if err != nil {
		return "", err
	}

	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", forge.Permanent(fmt.Errorf("parse token response: %w", err))
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
		return "", fmt.Errorf("sign JWT: %w", err)
	}
	return signed, nil
}

// apiRequest describes one call to the GitHub API.
type apiRequest struct {
	// op names the operation in the error text.
	op     string
	method string
	url    string
	// token goes in the Authorization header as a bearer token.
	token string
	// accept is the Accept header. Empty means the GitHub JSON media type.
	accept string
	// body is encoded as JSON when it is not nil.
	body any
	// want is the status that means success. Every other status is an error.
	want int
}

// do sends r and returns the response body when GitHub answers with r.want.
//
// The errors fall into three groups, so that a caller can decide whether to
// try again:
//   - a request that cannot be built or a body that cannot be encoded is
//     wrapped with forge.Permanent, because a retry builds the same request;
//   - a failure to reach GitHub or to read its answer is returned as it is,
//     which forge.IsTransient reports as transient;
//   - any other status is a *forge.APIError that carries the status.
func (g *GitHub) do(ctx context.Context, r apiRequest) ([]byte, error) {
	var reqBody io.Reader
	if r.body != nil {
		data, err := json.Marshal(r.body)
		if err != nil {
			return nil, forge.Permanent(fmt.Errorf("%s: marshal request: %w", r.op, err))
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, r.method, r.url, reqBody)
	if err != nil {
		return nil, forge.Permanent(fmt.Errorf("%s: build request: %w", r.op, err))
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	accept := r.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: send request: %w", r.op, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", r.op, err)
	}
	if resp.StatusCode != r.want {
		return nil, newAPIError(r.op, resp, body, time.Now())
	}
	return body, nil
}

// newAPIError describes a response that was not a success. It reads the
// rate-limit headers because GitHub answers a rate limit with 403 as well as
// 429, and a 403 is otherwise a refusal that never changes on a retry.
func newAPIError(op string, resp *http.Response, body []byte, now time.Time) *forge.APIError {
	apiErr := &forge.APIError{
		Op:         op,
		StatusCode: resp.StatusCode,
		Body:       string(body),
	}

	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), now)
	apiErr.RetryAfter = retryAfter

	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return apiErr
	}

	exhausted := strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0"
	if !hasRetryAfter && !exhausted {
		return apiErr
	}
	apiErr.RateLimited = true

	// The primary rate limit gives the time it resets instead of a delay.
	if !hasRetryAfter {
		if reset, err := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			apiErr.RetryAfter = max(time.Unix(reset, 0).Sub(now), 0)
		}
	}
	return apiErr
}

// parseRetryAfter reads a Retry-After header, which is either a number of
// seconds or an HTTP date. The second result is false when the header is
// absent or cannot be read. A date in the past gives zero.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}

// parseRepo is ParseRepoURL for the API methods. A URL that cannot be parsed
// never can be, so the error is permanent.
func parseRepo(repoURL string) (owner, repo string, err error) {
	owner, repo, err = ParseRepoURL(repoURL)
	if err != nil {
		return "", "", forge.Permanent(err)
	}
	return owner, repo, nil
}

// repoAPI returns the API URL of the repository, with each name escaped so a
// strange name cannot change the path.
func (g *GitHub) repoAPI(owner, repo string) string {
	return fmt.Sprintf("%s/repos/%s/%s", g.apiBase, url.PathEscape(owner), url.PathEscape(repo))
}

// maxTitleLength is GitHub's hard limit on a pull request title, counted in
// characters. A longer title is rejected with 422, which would throw away a
// completed run at the last step — the branch is already pushed by then, but
// nothing points at it.
const maxTitleLength = 256

// clampTitle cuts a title down to what the API will accept, marking the cut so
// a reader can tell the title is not the whole sentence.
//
// This is a backstop for a title no earlier layer bounded, not the place style
// is enforced: a repository's configured budget is a fraction of this, and a
// title that overruns it is still sent as written.
func clampTitle(title string) string {
	runes := []rune(title)
	if len(runes) <= maxTitleLength {
		return title
	}
	slog.Warn("truncating pull request title to the GitHub limit",
		"length", len(runes), "limit", maxTitleLength)
	// One rune short of the limit leaves room for the ellipsis.
	return strings.TrimRight(string(runes[:maxTitleLength-1]), " \t\n") + "…"
}

// CreatePullRequest opens a pull request. A 422 comes back as an APIError
// like every other refusal: GitHub uses it both for a request it cannot
// accept and for a pull request that already exists, and only the caller
// knows whether to look for the existing one with FindPullRequest.
func (g *GitHub) CreatePullRequest(ctx context.Context, opts forge.CreatePROpts) (*forge.PullRequest, error) {
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return nil, err
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return nil, err
	}

	respBody, err := g.do(ctx, apiRequest{
		op:     "create PR",
		method: http.MethodPost,
		url:    g.repoAPI(owner, repo) + "/pulls",
		token:  creds.APIToken(),
		body: map[string]any{
			"title": clampTitle(opts.Title),
			"body":  opts.Body,
			"head":  opts.HeadBranch,
			"base":  opts.BaseBranch,
		},
		want: http.StatusCreated,
	})
	if err != nil {
		return nil, err
	}

	var prResp struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &prResp); err != nil {
		return nil, forge.Permanent(fmt.Errorf("parse PR response: %w", err))
	}

	slog.Info("pull request created",
		"url", prResp.HTMLURL,
		"number", prResp.Number,
	)

	return &forge.PullRequest{
		URL: prResp.HTMLURL,
		Ref: strconv.Itoa(prResp.Number),
	}, nil
}

// FindPullRequest returns the newest pull request from HeadBranch in the
// repository itself. The head filter names the repository owner, so a branch
// with the same name on a fork does not match.
func (g *GitHub) FindPullRequest(ctx context.Context, opts forge.FindPROpts) (*forge.PullRequest, error) {
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return nil, err
	}
	if opts.HeadBranch == "" {
		return nil, forge.Permanent(errors.New("find PR: head branch is empty"))
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	query.Set("head", owner+":"+opts.HeadBranch)
	query.Set("state", "all")
	query.Set("sort", "created")
	query.Set("direction", "desc")
	query.Set("per_page", "1")

	respBody, err := g.do(ctx, apiRequest{
		op:     "find PR",
		method: http.MethodGet,
		url:    g.repoAPI(owner, repo) + "/pulls?" + query.Encode(),
		token:  creds.APIToken(),
		want:   http.StatusOK,
	})
	if err != nil {
		return nil, err
	}

	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &prs); err != nil {
		return nil, forge.Permanent(fmt.Errorf("parse PR list response: %w", err))
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &forge.PullRequest{
		URL: prs[0].HTMLURL,
		Ref: strconv.Itoa(prs[0].Number),
	}, nil
}

// diffCap bounds the diff handed back to callers. A PR diff feeds an agent
// prompt, so an unbounded one would blow the context window; the cap mirrors
// the session event-payload cap.
const diffCap = 256 * 1024

// diffTruncationMarker marks a diff cut short by diffCap.
const diffTruncationMarker = "\n…[diff truncated]\n"

// prNumber converts a forge-opaque PR ref into the numeric path segment the
// GitHub API expects. This is the one place that knows GitHub refs are numbers.
func prNumber(ref string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(ref))
	if err != nil || n <= 0 {
		return 0, forge.Permanent(fmt.Errorf("invalid GitHub pull request ref %q: expected a positive number", ref))
	}
	return n, nil
}

func (g *GitHub) GetPullRequest(ctx context.Context, opts forge.GetPROpts) (*forge.PullRequestDetails, error) {
	body, err := g.getPullRequest(ctx, opts, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}

	var prResp struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if err := json.Unmarshal(body, &prResp); err != nil {
		return nil, forge.Permanent(fmt.Errorf("parse pull request response: %w", err))
	}

	// GitHub reports a merged PR as state "closed" plus merged=true; surface
	// the distinction so callers can tell "merged" from "closed unmerged".
	state := prResp.State
	if prResp.Merged {
		state = "merged"
	}

	details := &forge.PullRequestDetails{
		Ref:        strconv.Itoa(prResp.Number),
		State:      state,
		HeadBranch: prResp.Head.Ref,
		HeadSHA:    prResp.Head.SHA,
		BaseBranch: prResp.Base.Ref,
		Title:      prResp.Title,
		Body:       prResp.Body,
		URL:        prResp.HTMLURL,
	}
	// head.repo is null when the fork it came from has been deleted.
	if prResp.Head.Repo != nil {
		details.HeadRepo = prResp.Head.Repo.FullName
	}
	if prResp.Base.Repo != nil {
		details.BaseRepo = prResp.Base.Repo.FullName
	}
	return details, nil
}

func (g *GitHub) GetPullRequestDiff(ctx context.Context, opts forge.GetPROpts) (string, error) {
	body, err := g.getPullRequest(ctx, opts, "application/vnd.github.v3.diff")
	if err != nil {
		return "", err
	}
	if len(body) > diffCap {
		return string(body[:diffCap]) + diffTruncationMarker, nil
	}
	return string(body), nil
}

// getPullRequest performs GET /repos/{owner}/{repo}/pulls/{number} with the
// given Accept header and returns the raw response body. The JSON and diff
// representations differ only in that header.
func (g *GitHub) getPullRequest(ctx context.Context, opts forge.GetPROpts, accept string) ([]byte, error) {
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return nil, err
	}
	number, err := prNumber(opts.PRRef)
	if err != nil {
		return nil, err
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return nil, err
	}

	return g.do(ctx, apiRequest{
		op:     fmt.Sprintf("get pull request %d", number),
		method: http.MethodGet,
		url:    fmt.Sprintf("%s/pulls/%d", g.repoAPI(owner, repo), number),
		token:  creds.APIToken(),
		accept: accept,
		want:   http.StatusOK,
	})
}

func (g *GitHub) PostComment(ctx context.Context, opts forge.PostCommentOpts) error {
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return err
	}
	number, err := prNumber(opts.PRRef)
	if err != nil {
		return err
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return err
	}

	_, err = g.do(ctx, apiRequest{
		op:     "post comment",
		method: http.MethodPost,
		url:    fmt.Sprintf("%s/issues/%d/comments", g.repoAPI(owner, repo), number),
		token:  creds.APIToken(),
		body:   map[string]any{"body": opts.Body},
		want:   http.StatusCreated,
	})
	return err
}

// commentsPerPage is the largest page GitHub serves, so a search for a
// comment makes as few requests as it can.
const commentsPerPage = 100

// maxCommentPages bounds the search for a comment. A pull request with more
// comments than this since the search started is not normal, and searching
// all of it would cost more of the rate limit than a duplicate comment costs.
const maxCommentPages = 20

// HasComment reports whether a comment on the pull request contains
// opts.Marker. GitHub filters on the time a comment was last changed, which
// includes the comments created or edited at or after opts.Since.
func (g *GitHub) HasComment(ctx context.Context, opts forge.HasCommentOpts) (bool, error) {
	if opts.Marker == "" {
		return false, forge.Permanent(errors.New("search comments: marker is empty"))
	}
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return false, err
	}
	number, err := prNumber(opts.PRRef)
	if err != nil {
		return false, err
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return false, err
	}

	query := url.Values{}
	if !opts.Since.IsZero() {
		query.Set("since", opts.Since.UTC().Format(time.RFC3339))
	}
	query.Set("per_page", strconv.Itoa(commentsPerPage))

	for page := 1; page <= maxCommentPages; page++ {
		query.Set("page", strconv.Itoa(page))
		respBody, err := g.do(ctx, apiRequest{
			op:     "list comments",
			method: http.MethodGet,
			url:    fmt.Sprintf("%s/issues/%d/comments?%s", g.repoAPI(owner, repo), number, query.Encode()),
			token:  creds.APIToken(),
			want:   http.StatusOK,
		})
		if err != nil {
			return false, err
		}

		var comments []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(respBody, &comments); err != nil {
			return false, forge.Permanent(fmt.Errorf("parse comment list response: %w", err))
		}
		for _, c := range comments {
			if strings.Contains(c.Body, opts.Marker) {
				return true, nil
			}
		}
		if len(comments) < commentsPerPage {
			return false, nil
		}
	}

	slog.Warn("stopped searching pull request comments at the page limit",
		"repo", owner+"/"+repo, "pr", number, "pages", maxCommentPages)
	return false, nil
}

func (g *GitHub) UpdatePullRequest(ctx context.Context, opts forge.UpdatePROpts) error {
	if opts.Title == nil && opts.Body == nil {
		return nil
	}
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return err
	}
	number, err := prNumber(opts.PRRef)
	if err != nil {
		return err
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return err
	}

	// Only the fields the caller set go in the request. GitHub keeps every
	// field a PATCH leaves out, which is what lets a run that owns only the
	// body leave a title somebody else wrote alone.
	body := map[string]any{}
	if opts.Title != nil {
		body["title"] = clampTitle(*opts.Title)
	}
	if opts.Body != nil {
		body["body"] = *opts.Body
	}

	_, err = g.do(ctx, apiRequest{
		op:     "update PR",
		method: http.MethodPatch,
		url:    fmt.Sprintf("%s/pulls/%d", g.repoAPI(owner, repo), number),
		token:  creds.APIToken(),
		body:   body,
		want:   http.StatusOK,
	})
	return err
}

// AddLabels adds labels to a pull request. GitHub keeps the labels the pull
// request already has and ignores one it has twice, so a repeated call
// changes nothing.
func (g *GitHub) AddLabels(ctx context.Context, opts forge.AddLabelsOpts) error {
	if len(opts.Labels) == 0 {
		return nil
	}
	owner, repo, err := parseRepo(opts.RepoURL)
	if err != nil {
		return err
	}
	number, err := prNumber(opts.PRRef)
	if err != nil {
		return err
	}

	creds, err := scm.Resolve(ctx, opts.Credentials)
	if err != nil {
		return err
	}

	_, err = g.do(ctx, apiRequest{
		op:     "add labels",
		method: http.MethodPost,
		url:    fmt.Sprintf("%s/issues/%d/labels", g.repoAPI(owner, repo), number),
		token:  creds.APIToken(),
		body:   map[string]any{"labels": opts.Labels},
		want:   http.StatusOK,
	})
	return err
}

// ParseRepoURL extracts the owner and repo name from a GitHub URL.
// Supports HTTPS (https://github.com/owner/repo) and SSH (git@github.com:owner/repo) formats.
func ParseRepoURL(repoURL string) (owner, repo string, err error) {
	// SSH format: git@github.com:owner/repo.git
	if strings.HasPrefix(repoURL, "git@") {
		parts := strings.SplitN(repoURL, ":", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("invalid SSH URL: %s", repoURL)
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
				return "", "", fmt.Errorf("invalid HTTPS URL: %s", repoURL)
			}
			path = path[slashIdx+1:]
			path = strings.TrimSuffix(path, ".git")
			return splitOwnerRepo(path, repoURL)
		}
	}

	return "", "", fmt.Errorf("unsupported URL format: %s", repoURL)
}

func splitOwnerRepo(path, originalURL string) (string, string, error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot extract owner/repo from URL: %s", originalURL)
	}
	return parts[0], parts[1], nil
}
