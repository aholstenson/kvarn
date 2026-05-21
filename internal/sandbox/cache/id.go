package cache

import (
	"crypto/sha256"
	"encoding/hex"
)

// ProjectID returns a stable, filesystem-safe identifier derived from the
// absolute path to a project directory. The result is a 16-character hex string.
func ProjectID(absPath string) string {
	h := sha256.Sum256([]byte(absPath))
	return hex.EncodeToString(h[:8])
}
