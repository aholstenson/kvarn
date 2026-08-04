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

// CredentialSource yields credentials that are valid at the moment of the call.
//
// Some forge credentials expire: a GitHub App installation token lives for an
// hour, while a job may run longer than that before it pushes. Operations
// therefore carry a source rather than a token captured at job start, and
// resolve it immediately before authenticating.
type CredentialSource interface {
	Credentials(ctx context.Context) (*Credentials, error)
}

// StaticCredentials wraps credentials that never expire, such as a personal
// access token or an SSH key. A nil argument yields a nil source, so callers
// can pass through "no credentials configured" unchanged.
func StaticCredentials(creds *Credentials) CredentialSource {
	if creds == nil {
		return nil
	}
	return staticSource{creds: creds}
}

type staticSource struct {
	creds *Credentials
}

func (s staticSource) Credentials(context.Context) (*Credentials, error) {
	return s.creds, nil
}

// Resolve reads credentials from a source, treating a nil source as "no
// credentials" rather than an error.
func Resolve(ctx context.Context, src CredentialSource) (*Credentials, error) {
	if src == nil {
		return nil, nil
	}
	return src.Credentials(ctx)
}

// DefaultCloneDepth bounds history fetched at job start. Deep enough for
// `git log`/`git blame` to give the agent meaningful context, shallow enough
// to skip the long tail of a multi-year repo.
const DefaultCloneDepth = 100

// CloneOpts configures a clone operation.
type CloneOpts struct {
	URL         string
	Branch      string
	Credentials CredentialSource
	Destination string
	Depth       int // 0 = full, >0 = shallow
}

// CommitAndPushOpts configures a commit-and-push operation on a host-side clone.
type CommitAndPushOpts struct {
	RepoDir string // host-side clone directory
	// RemoteURL is the push target, stated explicitly rather than read back
	// off the clone's "origin". Clone deliberately leaves no remote behind, so
	// there is nothing to read back; naming the target here is also what lets
	// a clone taken from a local mirror still push to the real forge.
	RemoteURL   string
	Branch      string // new branch name to create and push
	Message     string // commit message
	AuthorName  string
	AuthorEmail string
	Credentials CredentialSource
}

// APIToken returns a token suitable for forge API calls. It prefers the
// explicit Token field but falls back to Password (common when a PAT is
// stored as username/password basic auth). Nil-safe, so callers can ask
// without first checking whether credentials were configured at all.
func (c *Credentials) APIToken() string {
	if c == nil {
		return ""
	}
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
