//go:build linux

package runner

import (
	"os/user"
	"strconv"

	"github.com/cockroachdb/errors"
)

// kvarnCredential holds the cached UID/GID for the kvarn user.
type kvarnCredential struct {
	uid uint32
	gid uint32
}

// lookupKvarnUser resolves the kvarn user's UID and GID.
// Returns nil when already running as the kvarn user (no privilege change needed).
func lookupKvarnUser() (*kvarnCredential, error) {
	u, err := user.Lookup("kvarn")
	if err != nil {
		return nil, errors.Wrap(err, "lookup kvarn user")
	}

	// If we're already running as the kvarn user, no privilege change is needed.
	current, err := user.Current()
	if err == nil && current.Uid == u.Uid {
		return nil, nil
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, errors.Wrap(err, "parse uid")
	}

	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, errors.Wrap(err, "parse gid")
	}

	return &kvarnCredential{
		uid: uint32(uid),
		gid: uint32(gid),
	}, nil
}

// chownIDs returns the UID and GID for chown operations.
func (c *kvarnCredential) chownIDs() (int, int) {
	return int(c.uid), int(c.gid)
}
