package git

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// pinnedHostFingerprints maps hostname → accepted SHA256 SSH host key
// fingerprints (the `ssh.FingerprintSHA256` form, i.e. "SHA256:..."). Sourced
// from each provider's official docs and verified against their published
// values.
//
// This table is the human-verifiable authority; embeddedHostKeys carries the
// same keys in the full form OpenSSH needs. hostkey_test.go asserts the two
// agree, so a wrong or tampered key blob cannot pass review.
//
// Update both when a provider rotates its host keys.
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

// embeddedHostKeys holds the pinned keys as known_hosts lines. Fingerprints
// alone cannot be pinned through OpenSSH: known_hosts matches on the full
// public key, so the key material has to travel with kvarn.
//
//go:embed hostkeys.txt
var embeddedHostKeys string

// writeKnownHosts assembles the known_hosts file one ssh invocation will use
// and writes it into dir, returning its path.
//
// It is the pinned keys first and then the user's own ~/.ssh/known_hosts, which
// reproduces the pinning semantics kvarn has always had: a pinned host verifies
// against kvarn's copy of its key, any other host verifies against whatever the
// operator has already trusted, and an unknown host is refused rather than
// accepted on first sight.
func writeKnownHosts(dir string) (string, error) {
	var b strings.Builder
	b.WriteString(embeddedHostKeys)
	if !strings.HasSuffix(embeddedHostKeys, "\n") {
		b.WriteString("\n")
	}

	if path := defaultKnownHostsPath(); path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			b.WriteString("\n# --- operator's ~/.ssh/known_hosts ---\n")
			b.Write(data)
			if len(data) > 0 && data[len(data)-1] != '\n' {
				b.WriteString("\n")
			}
		case errors.Is(err, os.ErrNotExist):
			// No known_hosts file — fine, only pinned hosts will verify.
		default:
			slog.Warn("could not read known_hosts, only pinned hosts will verify",
				"path", path, "error", err)
		}
	}

	out := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(out, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("write known_hosts: %w", err)
	}
	return out, nil
}

func defaultKnownHostsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}
