package forge

import (
	"context"

	"github.com/aholstenson/kvarn/internal/scm"
)

// Forge abstracts a code hosting platform (GitHub, GitLab, etc.).
type Forge interface {
	// SCM returns a source control manager configured for this forge.
	SCM() scm.SCM

	// ResolveCredentials interprets forge-specific credential config and
	// returns a source of SCM-level credentials (tokens, SSH keys, etc.).
	// A source rather than a fixed value because forge credentials can expire
	// while a job runs; each operation re-reads it when it authenticates.
	ResolveCredentials(ctx context.Context, config map[string]string) (scm.CredentialSource, error)

	// ResolveCloneURL expands a repo reference into a full clone URL.
	// GitHub: "org/repo" -> "https://github.com/org/repo.git"
	// Noop: returns the input as-is (expects full URL).
	ResolveCloneURL(repo string) (string, error)

	// CreatePullRequest opens a PR on the platform.
	CreatePullRequest(ctx context.Context, opts CreatePROpts) (*PullRequest, error)

	// GetPullRequest looks up an existing PR by its forge-specific ref.
	GetPullRequest(ctx context.Context, opts GetPROpts) (*PullRequestDetails, error)

	// GetPullRequestDiff returns the PR's diff in unified format. Large diffs
	// may be truncated by the implementation.
	GetPullRequestDiff(ctx context.Context, opts GetPROpts) (string, error)

	// PostComment posts a comment on an existing PR or issue.
	PostComment(ctx context.Context, opts PostCommentOpts) error
}

// CreatePROpts configures PR creation.
type CreatePROpts struct {
	RepoURL     string
	BaseBranch  string
	HeadBranch  string
	Title       string
	Body        string
	Labels      []string
	Credentials scm.CredentialSource
}

// GetPROpts identifies a pull request to read. PRRef is opaque to kvarn — each
// forge interprets its own format (GitHub: the decimal PR number).
type GetPROpts struct {
	RepoURL     string
	PRRef       string
	Credentials scm.CredentialSource
}

// PostCommentOpts configures posting a comment on a PR or issue.
type PostCommentOpts struct {
	RepoURL     string
	PRRef       string
	Body        string
	Credentials scm.CredentialSource
}

// PullRequest holds information about a created PR.
type PullRequest struct {
	URL string
	// Ref identifies the PR to later Forge calls. Opaque to kvarn.
	Ref string
}

// PullRequestDetails describes an existing pull request. HeadRepo and BaseRepo
// let callers detect a fork PR, whose head branch lives in another repository
// and therefore cannot be pushed to.
type PullRequestDetails struct {
	Ref string
	// State is "open", "closed", or "merged".
	State      string
	HeadBranch string
	HeadSHA    string
	HeadRepo   string
	BaseBranch string
	BaseRepo   string
	Title      string
	Body       string
	URL        string
}
