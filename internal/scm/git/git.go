// Package git implements scm.SCM on top of the git command line.
//
// The CLI rather than a library because the operations kvarn depends on —
// shallow single-branch clones, narrow refspec fetches, protocol-v2 ref
// filtering — are what git itself is fastest and most complete at, and because
// keeping credentials out of every persistent artifact is easier to guarantee
// against a process boundary than against an in-process object graph. See
// auth.go for how credentials are passed, and run.go for why every
// caller-supplied value is an operand.
package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/logging"
	"github.com/aholstenson/kvarn/internal/scm"
)

// Git implements the scm.SCM interface using the git command line.
type Git struct{}

func (g *Git) Clone(ctx context.Context, opts scm.CloneOpts) error {
	if opts.URL == "" {
		return errors.New("clone URL is required")
	}
	if opts.Destination == "" {
		return errors.New("destination is required")
	}

	auth, err := ResolveAuth(ctx, opts.URL, opts.Credentials)
	if err != nil {
		return err
	}
	defer auth.Close()

	var flags []string
	if opts.Branch != "" {
		flags = append(flags, "--branch", opts.Branch, "--single-branch")
	}
	if opts.Depth > 0 {
		flags = append(flags, "--depth", strconv.Itoa(opts.Depth))
	}

	log := slog.With("url", RedactURL(opts.URL), "branch", opts.Branch, "depth", opts.Depth)
	log.Info("cloning from remote", "destination", opts.Destination)
	start := time.Now()

	if _, err := Run(ctx, Cmd{
		Config:   auth.Config,
		Env:      auth.Env,
		Sub:      "clone",
		Flags:    flags,
		Operands: []string{opts.URL, opts.Destination},
	}); err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	if err := SanitizeClone(ctx, opts.Destination); err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	log.Info("cloned from remote", "duration", logging.Elapsed(start))
	return nil
}

// SanitizeClone strips a fresh clone of everything that records where it came
// from, leaving a repository that is complete but has no upstream.
//
// This is what makes it safe to ship the repository into a VM. A project may be
// configured with a URL that embeds a credential, and git writes that URL into
// two places: the remote in .git/config, and the "clone: from <url>" entry in
// the reflog. Removing the remote alone would leave the reflog copy behind.
//
// Nothing downstream wants the remote either: the VM never fetches or pushes,
// changes come back through ExtractChanges, and the host-side push names its
// target explicitly via CommitAndPushOpts.RemoteURL. With no remote there is no
// field a credential can hide in, which is a stronger guarantee than sanitizing
// one.
func SanitizeClone(ctx context.Context, dir string) error {
	if _, err := Run(ctx, Cmd{
		Dir:   dir,
		Sub:   "remote",
		Flags: []string{"remove", "origin"},
	}); err != nil {
		return fmt.Errorf("remove origin remote: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git", "logs")); err != nil {
		return fmt.Errorf("remove reflogs: %w", err)
	}
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
	if opts.RemoteURL == "" {
		return errors.New("remote URL is required")
	}

	branchRef := "refs/heads/" + opts.Branch

	// Point a new branch at the current commit and move HEAD onto it using
	// nothing but ref writes. `checkout -b` would reach for the worktree and
	// index, and the worktree here is deliberately dirty: it holds the files
	// ExtractChanges copied out of the VM, which are the entire point of the
	// commit about to be made.
	head, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Sub:      "rev-parse",
		Flags:    []string{"--verify"},
		Operands: []string{"HEAD"},
	})
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	headSHA := strings.TrimSpace(head)

	if _, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Sub:      "update-ref",
		Operands: []string{branchRef, headSHA},
	}); err != nil {
		return fmt.Errorf("create branch ref: %w", err)
	}
	if _, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Sub:      "symbolic-ref",
		Operands: []string{"HEAD", branchRef},
	}); err != nil {
		return fmt.Errorf("update HEAD: %w", err)
	}

	if _, err := Run(ctx, Cmd{
		Dir:   opts.RepoDir,
		Sub:   "add",
		Flags: []string{"-A"},
	}); err != nil {
		return fmt.Errorf("stage changes: %w", err)
	}

	// Identity comes from -c rather than global config, which the orchestrator
	// host may not have at all. The message arrives on stdin so an agent-written
	// body of arbitrary length and shape never has to survive argv.
	if _, err := Run(ctx, Cmd{
		Dir: opts.RepoDir,
		Config: []string{
			"user.name=" + opts.AuthorName,
			"user.email=" + opts.AuthorEmail,
			"commit.gpgsign=false",
		},
		Sub:   "commit",
		Flags: []string{"--no-verify", "-F", "-"},
		Stdin: opts.Message,
	}); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	slog.Info("committed changes",
		"branch", opts.Branch,
		"author", opts.AuthorName,
	)

	// Resolved here rather than reused from clone time: the job between the two
	// can outlive a short-lived token, and this is the last moment before the
	// network call.
	auth, err := ResolveAuth(ctx, opts.RemoteURL, opts.Credentials)
	if err != nil {
		return fmt.Errorf("configure push auth: %w", err)
	}
	defer auth.Close()

	if _, err := Run(ctx, Cmd{
		Dir:      opts.RepoDir,
		Config:   auth.Config,
		Env:      auth.Env,
		Sub:      "push",
		Operands: []string{opts.RemoteURL, branchRef + ":" + branchRef},
	}); err != nil {
		return fmt.Errorf("push: %w", err)
	}

	slog.Info("pushed branch", "branch", opts.Branch)
	return nil
}

// RedactURL renders a remote URL for a log line with any embedded credential
// removed. A project may be configured as https://user:token@host/repo.git, and
// a log file is as durable a place for a token to come to rest as a config file
// — which is the thing SanitizeClone exists to prevent. Every log line carrying
// a repository URL goes through this.
func RedactURL(raw string) string {
	// An opaque URL — one whose scheme is not followed by "//" — parses with its
	// whole body, userinfo included, in a single field, so it is handled below
	// with the other forms url.Parse does not decompose.
	if u, err := url.Parse(raw); err == nil && u.Opaque == "" {
		if u.User == nil {
			return raw
		}
		// Replacing the userinfo rather than dropping it keeps the line honest
		// about the URL being credentialed, which matters when diagnosing which
		// of two configurations a job actually used. The username goes too: a
		// GitHub App token is just as often spelled as the user.
		u.User = url.User("redacted")
		return u.String()
	}

	// git accepts forms url.Parse rejects, scp-style "git@host:path" chief
	// among them. There the user is part of the address rather than a secret —
	// ssh authenticates with a key — so only a password is worth hiding, and
	// keeping the rest legible is what makes the line useful.
	rest, prefix := raw, ""
	if i := strings.Index(raw, "://"); i >= 0 {
		prefix, rest = raw[:i+3], raw[i+3:]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return raw
	}
	if colon := strings.Index(rest[:at], ":"); colon >= 0 {
		return prefix + rest[:colon] + ":redacted" + rest[at:]
	}
	return raw
}

// isSSHURL returns true if the URL looks like an SSH git URL.
func isSSHURL(url string) bool {
	return strings.HasPrefix(url, "git@") ||
		strings.HasPrefix(url, "ssh://") ||
		strings.Contains(url, "@") && strings.Contains(url, ":")
}
