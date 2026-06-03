package scm

import "context"

// Credentials holds authentication details for accessing a repository.
type Credentials struct {
	Token      string
	SSHKey     []byte
	SSHKeyPass string
	Username   string
	Password   string
}

// DefaultCloneDepth bounds history fetched at job start. Deep enough for
// `git log`/`git blame` to give the agent meaningful context, shallow enough
// to skip the long tail of a multi-year repo.
const DefaultCloneDepth = 100

// CloneOpts configures a clone operation.
type CloneOpts struct {
	URL         string
	Branch      string
	Credentials *Credentials
	Destination string
	Depth       int // 0 = full, >0 = shallow
}

// CommitAndPushOpts configures a commit-and-push operation on a host-side clone.
type CommitAndPushOpts struct {
	RepoDir     string // host-side clone directory
	Branch      string // new branch name to create and push
	Message     string // commit message
	AuthorName  string
	AuthorEmail string
	Credentials *Credentials
}

// APIToken returns a token suitable for forge API calls. It prefers the
// explicit Token field but falls back to Password (common when a PAT is
// stored as username/password basic auth).
func (c *Credentials) APIToken() string {
	if c.Token != "" {
		return c.Token
	}
	return c.Password
}

// SCM abstracts source code management operations.
type SCM interface {
	Clone(ctx context.Context, opts CloneOpts) error
	CommitAndPush(ctx context.Context, opts CommitAndPushOpts) error
}
