package forgegit

import (
	"context"

	"github.com/aholstenson/kvarn/internal/forge"
	"github.com/aholstenson/kvarn/internal/scm"
	scmgit "github.com/aholstenson/kvarn/internal/scm/git"
	"github.com/cockroachdb/errors"
)

// Git is a forge implementation for plain git repositories without a hosting
// platform. It supports cloning and pushing but cannot create pull requests.
type Git struct{}

func New() *Git {
	return &Git{}
}

func (g *Git) SCM() scm.SCM {
	return &scmgit.Git{}
}

func (g *Git) ResolveCloneURL(repo string) (string, error) {
	return repo, nil
}

func (g *Git) ResolveCredentials(_ context.Context, config map[string]string) (*scm.Credentials, error) {
	creds := &scm.Credentials{}

	if token := config["token"]; token != "" {
		creds.Token = token
	}

	if sshKeyPath := config["ssh_key_path"]; sshKeyPath != "" {
		creds.SSHKey = []byte(sshKeyPath)
	}

	if sshKeyPass := config["ssh_key_pass"]; sshKeyPass != "" {
		creds.SSHKeyPass = sshKeyPass
	}

	if username := config["username"]; username != "" {
		creds.Username = username
	}

	if password := config["password"]; password != "" {
		creds.Password = password
	}

	return creds, nil
}

func (g *Git) CreatePullRequest(_ context.Context, _ forge.CreatePROpts) (*forge.PullRequest, error) {
	return nil, errors.New("pull request creation is not supported by the git forge")
}

func (g *Git) PostComment(_ context.Context, _ forge.PostCommentOpts) error {
	return errors.New("posting comments is not supported by the git forge")
}
