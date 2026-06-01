package git

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// pinnedHostFingerprints maps hostname → accepted SHA256 SSH host key
// fingerprints (the `ssh.FingerprintSHA256` form, i.e. "SHA256:..."). Sourced
// from each provider's official docs and verified against their published
// values.
//
// Update when a provider rotates its host keys.
var pinnedHostFingerprints = map[string][]string{
	// https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
	"github.com": {
		"SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU", // ed25519
		"SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM", // ecdsa
		"SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s", // rsa
	},
	// https://docs.gitlab.com/user/gitlab_com/
	"gitlab.com": {
		"SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8", // ed25519
		"SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw", // ecdsa
		"SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ", // rsa
	},
	// https://support.atlassian.com/bitbucket-cloud/docs/configure-ssh-and-two-step-verification/
	"bitbucket.org": {
		"SHA256:ybgmFkzwOSotHTHLJgHO0QN8L0xErw6vd0VhFA9m3SM", // ed25519
		"SHA256:FC73VB6C4OQLSCrjEayhMp9UMxS97caD/Yyi2bhW/J0", // ecdsa
		"SHA256:46OSHA1Rmj8E8ERTC6xkNcmGOw9oFxYr0WF6zWW8l1E", // rsa
	},
}

// newHostKeyCallback returns an ssh.HostKeyCallback that verifies the presented
// host key against pinnedHostFingerprints. For hosts not in the pinned list it
// falls back to the user's ~/.ssh/known_hosts file if one exists.
func newHostKeyCallback() ssh.HostKeyCallback {
	var fallback ssh.HostKeyCallback
	if path := defaultKnownHostsPath(); path != "" {
		cb, err := knownhosts.New(path)
		switch {
		case err == nil:
			fallback = cb
		case errors.Is(err, os.ErrNotExist):
			// No known_hosts file — fine, only pinned hosts will verify.
		default:
			slog.Warn("could not load known_hosts, only pinned hosts will verify",
				"path", path, "err", err)
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		host := hostname
		if h, _, err := net.SplitHostPort(hostname); err == nil {
			host = h
		}

		if pinned, ok := pinnedHostFingerprints[host]; ok {
			got := ssh.FingerprintSHA256(key)
			for _, want := range pinned {
				if got == want {
					return nil
				}
			}
			return fmt.Errorf("host key for %s does not match any pinned fingerprint (got %s)",
				host, got)
		}

		if fallback != nil {
			return fallback(hostname, remote, key)
		}
		return fmt.Errorf("host key for %s is not pinned and no known_hosts file is available (fingerprint %s); "+
			"add the host to ~/.ssh/known_hosts or pin it in kvarn",
			host, ssh.FingerprintSHA256(key))
	}
}

func defaultKnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}
