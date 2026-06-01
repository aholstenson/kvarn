package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/scm"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Git implements the scm.SCM interface using go-git.
type Git struct{}

func (g *Git) Clone(ctx context.Context, opts scm.CloneOpts) error {
	if opts.URL == "" {
		return errors.New("clone URL is required")
	}
	if opts.Destination == "" {
		return errors.New("destination is required")
	}

	cloneOpts := &gogit.CloneOptions{
		URL: opts.URL,
	}

	if opts.Branch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
		cloneOpts.SingleBranch = true
	}

	if opts.Depth > 0 {
		cloneOpts.Depth = opts.Depth
	}

	if opts.Credentials != nil {
		auth, err := authMethod(opts.URL, opts.Credentials)
		if err != nil {
			return fmt.Errorf("configure auth: %w", err)
		}
		cloneOpts.Auth = auth
	}

	slog.Info("cloning repository",
		"url", opts.URL,
		"branch", opts.Branch,
		"destination", opts.Destination,
		"depth", opts.Depth,
		"has_credentials", opts.Credentials != nil,
	)

	_, err := gogit.PlainCloneContext(ctx, opts.Destination, false, cloneOpts)
	if err != nil {
		if err.Error() == "invalid auth method" {
			if isSSHURL(opts.URL) && opts.Credentials != nil && (opts.Credentials.Token != "" || opts.Credentials.Username != "") {
				return fmt.Errorf("clone: auth method mismatch: URL %q is SSH but credential uses token/password (use ssh_key instead)", opts.URL)
			}
			if !isSSHURL(opts.URL) && opts.Credentials != nil && len(opts.Credentials.SSHKey) > 0 {
				return fmt.Errorf("clone: auth method mismatch: URL %q is HTTPS but credential uses ssh_key (use token instead)", opts.URL)
			}
		}
		return fmt.Errorf("clone: %w", err)
	}

	slog.Info("clone complete", "url", opts.URL)
	return nil
}

func (g *Git) CommitAndPush(ctx context.Context, opts scm.CommitAndPushOpts) error {
	if opts.RepoDir == "" {
		return errors.New("repo dir is required")
	}
	if opts.Branch == "" {
		return errors.New("branch is required")
	}
	if opts.Message == "" {
		return errors.New("commit message is required")
	}

	repo, err := gogit.PlainOpen(opts.RepoDir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("get worktree: %w", err)
	}

	// Create the new branch pointing at HEAD. We set HEAD to the new
	// branch and stage+commit without using Checkout, which would reset
	// the worktree/index and lose the dirty files written by ExtractChanges.
	branchRef := plumbing.NewBranchReferenceName(opts.Branch)
	headRef, err := repo.Head()
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	newRef := plumbing.NewHashReference(branchRef, headRef.Hash())
	if err := repo.Storer.SetReference(newRef); err != nil {
		return fmt.Errorf("create branch ref: %w", err)
	}
	// Point HEAD at the new branch.
	symRef := plumbing.NewSymbolicReference(plumbing.HEAD, branchRef)
	if err := repo.Storer.SetReference(symRef); err != nil {
		return fmt.Errorf("update HEAD: %w", err)
	}

	// Stage all changes.
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	// Commit.
	now := time.Now()
	_, err = wt.Commit(opts.Message, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  opts.AuthorName,
			Email: opts.AuthorEmail,
			When:  now,
		},
	})
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	slog.Info("committed changes",
		"branch", opts.Branch,
		"author", opts.AuthorName,
	)

	// Push.
	pushOpts := &gogit.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(branchRef + ":" + branchRef),
		},
	}
	if opts.Credentials != nil {
		// Resolve the remote URL so auth method selection (SSH vs HTTPS)
		// matches the actual remote.
		remoteURL := ""
		if remote, err := repo.Remote("origin"); err == nil {
			urls := remote.Config().URLs
			if len(urls) > 0 {
				remoteURL = urls[0]
			}
		}
		auth, err := authMethod(remoteURL, opts.Credentials)
		if err != nil {
			return fmt.Errorf("configure push auth: %w", err)
		}
		pushOpts.Auth = auth
	}

	if err := repo.PushContext(ctx, pushOpts); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	slog.Info("pushed branch", "branch", opts.Branch)
	return nil
}

// isSSHURL returns true if the URL looks like an SSH git URL.
func isSSHURL(url string) bool {
	return strings.HasPrefix(url, "git@") ||
		strings.HasPrefix(url, "ssh://") ||
		strings.Contains(url, "@") && strings.Contains(url, ":")
}

func authMethod(url string, creds *scm.Credentials) (transport.AuthMethod, error) {
	sshURL := isSSHURL(url)

	// SSH key auth — only valid for SSH URLs.
	if len(creds.SSHKey) > 0 {
		if !sshURL {
			slog.Warn("credential has SSH key but URL is not SSH, falling through to token/password auth",
				"url", url,
			)
		} else {
			keyData, err := resolveSSHKey(creds.SSHKey)
			if err != nil {
				return nil, fmt.Errorf("resolve ssh key: %w", err)
			}
			keys, err := ssh.NewPublicKeys("git", keyData, creds.SSHKeyPass)
			if err != nil {
				return nil, fmt.Errorf("ssh key: %w", err)
			}
			keys.HostKeyCallback = newHostKeyCallback()
			slog.Info("using SSH key auth")
			return keys, nil
		}
	}

	// Token auth — only valid for HTTP(S) URLs.
	if creds.Token != "" {
		if sshURL {
			slog.Warn("credential has token but URL is SSH, token auth is not supported for SSH URLs",
				"url", url,
			)
			return nil, nil
		}
		slog.Info("using token auth", "username", "x-access-token")
		return &http.BasicAuth{
			Username: "x-access-token",
			Password: creds.Token,
		}, nil
	}

	// Username/password auth — only valid for HTTP(S) URLs.
	if creds.Username != "" {
		if sshURL {
			slog.Warn("credential has username/password but URL is SSH",
				"url", url,
			)
			return nil, nil
		}
		slog.Info("using basic auth", "username", creds.Username)
		return &http.BasicAuth{
			Username: creds.Username,
			Password: creds.Password,
		}, nil
	}

	slog.Warn("credentials provided but no auth fields are set")
	return nil, nil
}

// resolveSSHKey handles the SSHKey field which can be either a file path or
// inline PEM data.
func resolveSSHKey(key []byte) ([]byte, error) {
	s := string(key)

	// If it looks like PEM data, use it directly.
	if strings.HasPrefix(strings.TrimSpace(s), "-----BEGIN") {
		return key, nil
	}

	// Otherwise treat it as a file path.
	expanded := os.ExpandEnv(s)
	if strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("expand home dir: %w", err)
		}
		expanded = home + expanded[1:]
	}

	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read key file %q: %w", expanded, err)
	}

	slog.Info("loaded SSH key from file", "path", expanded)
	return data, nil
}
