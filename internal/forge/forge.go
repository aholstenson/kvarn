package forge

import (
	"context"
	"time"

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

	// UpdatePullRequest changes the title, the body, or both of an existing
	// PR. A field left nil is not sent, so the forge keeps what it has.
	UpdatePullRequest(ctx context.Context, opts UpdatePROpts) error

	// FindPullRequest returns the newest pull request whose head is
	// HeadBranch in the repository itself, open or not, or nil when there is
	// none. A delivery uses it before it opens a pull request again, because
	// a create whose answer was lost may still have opened one.
	FindPullRequest(ctx context.Context, opts FindPROpts) (*PullRequest, error)

	// AddLabels adds labels to a pull request. Labels it already has are
	// kept, so calling it again with the same labels changes nothing.
	AddLabels(ctx context.Context, opts AddLabelsOpts) error

	// HasComment reports whether a comment on the pull request contains
	// Marker. Only comments created or edited at or after Since are searched.
	// A delivery uses it before it posts a comment again, because a post
	// whose answer was lost may still have landed.
	HasComment(ctx context.Context, opts HasCommentOpts) (bool, error)
}

// FindPROpts identifies the pull request to look for by its head branch.
type FindPROpts struct {
	RepoURL     string
	HeadBranch  string
	Credentials scm.CredentialSource
}

// AddLabelsOpts names the labels to add to a pull request.
type AddLabelsOpts struct {
	RepoURL     string
	PRRef       string
	Labels      []string
	Credentials scm.CredentialSource
}

// HasCommentOpts describes the comment to look for.
type HasCommentOpts struct {
	RepoURL     string
	PRRef       string
	Marker      string
	Since       time.Time
	Credentials scm.CredentialSource
}

// CreatePROpts configures PR creation.
type CreatePROpts struct {
	RepoURL     string
	BaseBranch  string
	HeadBranch  string
	Title       string
	Body        string
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

// UpdatePROpts configures a change to an existing PR's title and body.
//
// The fields are pointers because "leave it alone" and "set it to this" must
// stay apart: an update that only owns the body must not touch a title a
// person wrote.
type UpdatePROpts struct {
	RepoURL     string
	PRRef       string
	Title       *string
	Body        *string
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
