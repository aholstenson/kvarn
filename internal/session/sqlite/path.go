package sqlite

import (
	"os"
	"path/filepath"
)

// DefaultPath returns the standard sessions database location, mirroring the
// other stores under ~/.config/kvarn/.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "sessions.db")
}
