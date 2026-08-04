package git

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/aholstenson/kvarn/internal/scm"
	"golang.org/x/crypto/ssh"
)

// Names of the variables the inline credential helper reads. Only the names
// ever appear on a command line; the values live in the child's environment,
// which — unlike argv — is not world-readable.
const (
	envGitUser = "KVARN_GIT_USER"
	envGitPass = "KVARN_GIT_PASS"
)

// credentialHelper is a shell function git runs to obtain a username and
// password. It answers only the "get" action, so git can neither store nor
// erase through it.
//
// The secret reaches the helper through the environment rather than through
// this string, so a token never appears in argv where any local user could read
// it out of `ps`. And because `-c` config applies to a single invocation, it is
// never written to .git/config — which is what makes the repository shipped
// into the VM structurally incapable of carrying a credential.
const credentialHelper = `!f() { test "$1" = get && printf "username=%s\npassword=%s\n" ` +
	`"$` + envGitUser + `" "$` + envGitPass + `"; }; f`

// Auth is the credential material for one git invocation. Close must be called
// once the command has run; it removes any temporary key material.
type Auth struct {
	Config []string // -c arguments
	Env    []string // extra environment

	cleanup func()
}

// Close removes any temporary key material the auth created.
func (a *Auth) Close() {
	if a != nil && a.cleanup != nil {
		a.cleanup()
	}
}

func noopCleanup() {}

// ResolveAuth resolves credentials and turns them into the configuration and
// environment one git command needs to authenticate to remoteURL.
//
// Credentials are resolved here, at the last moment before the network call,
// rather than captured when the job started: a GitHub App installation token
// lives an hour and a job can outlive it.
//
// Which method was chosen is logged at debug: this runs once per git invocation
// — an ls-remote, a fetch, a clone, a push — and says nothing about what is
// being done, so at info it would bury the lines that do. The command that
// follows is what logs the operation.
func ResolveAuth(ctx context.Context, remoteURL string, src scm.CredentialSource) (*Auth, error) {
	creds, err := scm.Resolve(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials: %w", err)
	}

	log := slog.With("url", RedactURL(remoteURL))
	sshURL := isSSHURL(remoteURL)

	// Reset the helper chain on every HTTP(S) invocation, credentials or not.
	// A helper configured in the host's system or global git config would
	// otherwise be consulted — able to supply credentials kvarn did not choose
	// and, worse, to capture the ones it did.
	var config []string
	if !sshURL {
		config = append(config, "credential.helper=")
	}

	if creds == nil {
		log.Debug("authenticating anonymously; no credential is configured")
		return &Auth{Config: config, cleanup: noopCleanup}, nil
	}

	if sshURL {
		if len(creds.SSHKey) == 0 {
			if creds.Token != "" || creds.Username != "" {
				return nil, fmt.Errorf("auth method mismatch: URL %q is SSH but credential uses token/password (use ssh_key instead)", remoteURL)
			}
			log.Warn("credentials provided but no auth fields are set")
			return &Auth{cleanup: noopCleanup}, nil
		}
		return sshAuth(log, remoteURL, creds)
	}

	switch {
	case creds.Token != "":
		log.Debug("authenticating with a token", "username", "x-access-token")
		return &Auth{
			Config: append(config, "credential.helper="+credentialHelper),
			Env: []string{
				envGitUser + "=x-access-token",
				envGitPass + "=" + creds.Token,
			},
			cleanup: noopCleanup,
		}, nil
	case creds.Username != "":
		log.Debug("authenticating with a username and password", "username", creds.Username)
		return &Auth{
			Config: append(config, "credential.helper="+credentialHelper),
			Env: []string{
				envGitUser + "=" + creds.Username,
				envGitPass + "=" + creds.Password,
			},
			cleanup: noopCleanup,
		}, nil
	case len(creds.SSHKey) > 0:
		return nil, fmt.Errorf("auth method mismatch: URL %q is HTTPS but credential uses ssh_key (use token instead)", remoteURL)
	default:
		log.Warn("credentials provided but no auth fields are set")
		return &Auth{Config: config, cleanup: noopCleanup}, nil
	}
}

// sshAuth builds a GIT_SSH_COMMAND pinned to the credential's key and to
// kvarn's known_hosts, in a temporary directory the returned cleanup removes.
func sshAuth(log *slog.Logger, remoteURL string, creds *scm.Credentials) (_ *Auth, retErr error) {
	dir, err := os.MkdirTemp("", "kvarn-ssh-*")
	if err != nil {
		return nil, fmt.Errorf("create ssh temp dir: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }
	defer func() {
		if retErr != nil {
			cleanup()
		}
	}()
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure ssh temp dir: %w", err)
	}

	keyPath, err := resolveSSHKeyPath(log, dir, creds)
	if err != nil {
		return nil, err
	}
	knownHosts, err := writeKnownHosts(dir)
	if err != nil {
		return nil, err
	}

	// IdentitiesOnly plus IdentityAgent=none keeps a developer's ssh-agent from
	// silently offering a different identity than the one the credential names,
	// which would make "which key was this pushed with" unanswerable.
	parts := []string{
		"ssh",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-i", keyPath,
	}
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}

	log.Debug("authenticating with an SSH key")
	return &Auth{
		Env:     []string{"GIT_SSH_COMMAND=" + strings.Join(quoted, " ")},
		cleanup: cleanup,
	}, nil
}

// resolveSSHKeyPath yields a path to a private key ssh can use directly.
//
// A passphrase-protected key is decrypted in process and re-written without one
// into the per-invocation directory. The alternative — driving ssh's
// SSH_ASKPASS — needs a detached process group and hands the passphrase to a
// child's stdout, for no benefit over a plaintext key that exists inside a 0700
// directory for the length of one command.
func resolveSSHKeyPath(log *slog.Logger, dir string, creds *scm.Credentials) (string, error) {
	raw := string(creds.SSHKey)

	// The SSHKey field carries either inline PEM or a path; the plain-git forge
	// stores ssh_key_path into it, so both spellings have to keep working.
	inline := strings.HasPrefix(strings.TrimSpace(raw), "-----BEGIN")

	if !inline {
		path, err := expandKeyPath(raw)
		if err != nil {
			return "", err
		}
		if creds.SSHKeyPass == "" {
			log.Debug("using SSH key from file", "path", path)
			return path, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read key file %q: %w", path, err)
		}
		raw = string(data)
	}

	keyData := []byte(raw)
	if creds.SSHKeyPass != "" {
		parsed, err := ssh.ParseRawPrivateKeyWithPassphrase(keyData, []byte(creds.SSHKeyPass))
		if err != nil {
			return "", fmt.Errorf("decrypt ssh key: %w", err)
		}
		block, err := ssh.MarshalPrivateKey(normalizeKey(parsed), "")
		if err != nil {
			return "", fmt.Errorf("re-encode ssh key: %w", err)
		}
		keyData = pem.EncodeToMemory(block)
	}

	out := filepath.Join(dir, "id")
	if err := os.WriteFile(out, keyData, 0o600); err != nil {
		return "", fmt.Errorf("write ssh key: %w", err)
	}
	return out, nil
}

// normalizeKey unwraps the pointer form ParseRawPrivateKey returns for ed25519,
// which ssh.MarshalPrivateKey does not accept.
func normalizeKey(key any) any {
	if k, ok := key.(*ed25519.PrivateKey); ok {
		return *k
	}
	return key
}

// expandKeyPath resolves environment variables and a leading ~ in a key path.
func expandKeyPath(s string) (string, error) {
	expanded := os.ExpandEnv(s)
	if strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand home dir: %w", err)
		}
		expanded = filepath.Join(home, expanded[2:])
	}
	return expanded, nil
}

// shellQuote wraps s so git's shell-like splitting of GIT_SSH_COMMAND keeps it
// as one word, whatever the temp directory happens to be called.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
