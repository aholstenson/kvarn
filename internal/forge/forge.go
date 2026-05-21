package forge

import (
	"context"

	"github.com/aholstenson/kvarn/internal/scm"
)

// Forge abstracts a code hosting platform (GitHub, GitLab, etc.).
type Forge interface {
	// SCM returns a source control manager configured for this forge.
	SCM() scm.SCM

	// ResolveCredentials interprets forge-specific credential config
	// and returns SCM-level credentials (tokens, SSH keys, etc.).
	ResolveCredentials(ctx context.Context, config map[string]string) (*scm.Credentials, error)

	// ResolveCloneURL expands a repo reference into a full clone URL.
	// GitHub: "org/repo" -> "https://github.com/org/repo.git"
	// Noop: returns the input as-is (expects full URL).
	ResolveCloneURL(repo string) (string, error)

	// CreatePullRequest opens a PR on the platform.
	CreatePullRequest(ctx context.Context, opts CreatePROpts) (*PullRequest, error)

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
	Credentials *scm.Credentials
}

// PostCommentOpts configures posting a comment on a PR or issue.
type PostCommentOpts struct {
	RepoURL     string
	Number      int
	Body        string
	Credentials *scm.Credentials
}

// PullRequest holds information about a created PR.
type PullRequest struct {
	URL    string
	Number int
}
