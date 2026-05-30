package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// metadata is the JSON sidecar stored next to each <inputKey>.tar.zst.
//
// Filesystem atime is unreliable (noatime, relatime), so LastAccess is tracked
// here and bumped on every hit and warm-start.
type metadata struct {
	Namespace  string    `json:"namespace"`
	Bucket     string    `json:"bucket"`
	InputKey   string    `json:"inputKey"`
	GuestPath  string    `json:"guestPath"`
	Channel    string    `json:"channel,omitempty"`
	SizeBytes  int64     `json:"sizeBytes"`
	CreatedAt  time.Time `json:"createdAt"`
	LastAccess time.Time `json:"lastAccess"`
}

func readMetadata(path string) (*metadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse cache metadata %s: %w", path, err)
	}
	return &m, nil
}

func writeMetadata(path string, m *metadata) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal cache metadata: %w", err)
	}
	return atomicWrite(path, data, 0o644)
}

// atomicWrite writes data to path via a temp file in the same directory
// followed by rename, so readers never observe a partial file.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
