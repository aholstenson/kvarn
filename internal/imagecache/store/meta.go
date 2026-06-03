package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// blobMeta is the JSON sidecar stored next to each blob payload.
//
// LastAccess is tracked here (not via filesystem atime, which is unreliable
// under noatime/relatime) and bumped on every cache hit so the LRU sweep can
// pick true cold entries.
type blobMeta struct {
	Digest     string    `json:"digest"`
	SizeBytes  int64     `json:"sizeBytes"`
	CreatedAt  time.Time `json:"createdAt"`
	LastAccess time.Time `json:"lastAccess"`
}

// manifestMeta is the sidecar for a cached manifest. ResolvedDigest holds the
// content-addressable form (sha256:...) so a tag lookup can be translated
// into a digest hit without re-fetching upstream.
//
// IsTag distinguishes mutable tag references (which carry an ExpiresAt) from
// digest references (which never expire).
type manifestMeta struct {
	Registry       string    `json:"registry"`
	Name           string    `json:"name"`
	Ref            string    `json:"ref"`
	ResolvedDigest string    `json:"resolvedDigest"`
	ContentType    string    `json:"contentType"`
	SizeBytes      int64     `json:"sizeBytes"`
	IsTag          bool      `json:"isTag"`
	FetchedAt      time.Time `json:"fetchedAt"`
	ExpiresAt      time.Time `json:"expiresAt,omitempty"`
	LastAccess     time.Time `json:"lastAccess"`
	ETag           string    `json:"etag,omitempty"`
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse meta %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	return atomicWrite(path, data, 0o644)
}

// atomicWrite writes data via a temp file in the same directory followed by
// rename, so readers never observe a partial file.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
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
