// Package localsock owns the orchestrator's host-local control socket: where
// it lives, how it is created, and how a client dials it.
//
// It is its own package because both ends need the same answers — the
// orchestrator binds the socket, the CLI dials it — and a path constant
// duplicated across the two would be a path constant that drifts.
package localsock

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scheme marks an address as a path to the control socket rather than a URL.
// Only this explicit form is treated as a socket: inferring it from the shape
// of a string would let "does this look like a path" decide which transport a
// command uses.
const Scheme = "unix://"

// DefaultPath is where the control socket lives when the operator names none.
// It sits beside the other per-user orchestrator state (apikeys.toml,
// sessions.db) because it is protected by exactly the same thing those are:
// ownership of that directory.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "orchestrator.sock")
}

// dirMode / sockMode are what make the socket an authentication mechanism
// rather than an open door. The directory is narrowed first because a socket is
// briefly world-reachable between bind and chmod; creating it inside a
// directory no other account may traverse closes that window rather than racing
// it.
const (
	dirMode  = 0o700
	sockMode = 0o600
	dialWait = time.Second
)

// Listen creates the control socket at path.
//
// A unix socket rather than a loopback port, deliberately. "The request came
// from 127.0.0.1" is not an authentication claim — any local process can make
// it, and so can an SSRF or a rebound browser tab. A socket's permissions are
// enforced by the kernel and cannot be reached from off the host at all.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create socket directory %s: %w", dir, err)
	}

	// A socket file cannot be bound over, but one a live orchestrator is
	// serving must not be removed either — that would leave the running process
	// listening on a path nothing can reach. Dialling tells the two apart: a
	// refused connection means the file outlived its process.
	if _, err := os.Stat(path); err == nil {
		conn, derr := net.DialTimeout("unix", path, dialWait)
		if derr == nil {
			conn.Close()
			return nil, fmt.Errorf("another orchestrator is already serving %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, sockMode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("restrict socket permissions on %s: %w", path, err)
	}
	return listener, nil
}

// Exists reports whether path is a socket that could be dialled. It is what
// lets a CLI command prefer the local transport without being given a flag.
func Exists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// Address renders path as a dialable address.
func Address(path string) string { return Scheme + path }

// Path returns the socket path in addr, and whether addr named one at all.
func Path(addr string) (string, bool) { return strings.CutPrefix(addr, Scheme) }

// HTTPClient returns an HTTP client whose every connection goes to the socket
// at path. HTTP/1.1 is enough: the orchestrator serves the socket through the
// same h2c handler as its network listener, which falls through to the wrapped
// handler for non-h2 requests.
func HTTPClient(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}
}
