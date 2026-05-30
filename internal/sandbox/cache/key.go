package cache

// Key identifies a content-addressed cache entry.
//
// Storage is addressed by (ProjectID, Namespace, Bucket, InputKey). GuestPath
// is carried so the transfer layer knows where to extract/tar, but it is not
// part of the storage address.
type Key struct {
	// ProjectID is the branch-independent project identifier.
	ProjectID string
	// Namespace partitions pools; "" is the shared pool. A future
	// "pr-<n>" isolates untrusted fork PRs.
	Namespace string
	// Bucket is the cross-branch sharing unit: a tool name (go, cargo,
	// nix-eval, …) or "user:<flatpath>".
	Bucket string
	// GuestPath is the absolute guest path the tarball extracts to.
	GuestPath string
	// InputKey is the content address within a bucket:
	// hex(sha256(lockfileDigest || channel)).
	InputKey string
}

// Layer pairs a derived Key with the guest path it restores to.
type Layer struct {
	Key       Key
	GuestPath string
}
