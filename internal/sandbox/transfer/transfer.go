package transfer

import (
	"context"
	"io"
	"path/filepath"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
)

// FileUploader is the subset of RunnerProxy needed for batch file transfer.
type FileUploader interface {
	UploadFiles(ctx context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error)
}

// Uploader extends FileUploader with streaming and exec capabilities.
type Uploader interface {
	FileUploader
	StreamToGuest(ctx context.Context, destPath string, src io.Reader, size int64) error
	Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error)
}

// Options configures a single Upload call.
//
// Both hooks are per-call rather than fields on the Transferer because one
// Transferer instance is shared by every concurrent job in the orchestrator;
// per-instance hooks would have each job overwriting the others' callbacks.
type Options struct {
	// SkipFile, if non-nil, is called for each file and directory encountered
	// during the walk. If it returns true for a directory, the entire subtree
	// is skipped. If it returns true for a file, that file is not transferred.
	SkipFile func(relPath string, isDir bool) bool

	// OnProgress, if non-nil, is called as bytes are transferred with the
	// cumulative count and the total to transfer.
	OnProgress func(bytesSent, totalBytes int64)
}

// Transferer uploads local files to a remote VM via an Uploader.
type Transferer interface {
	Upload(ctx context.Context, uploader Uploader, localDir string, remoteDir string, opts Options) error
}

// GitDirOnlyFilter returns a SkipFile that admits only the repository itself:
// the ".git" directory and everything under it. Everything else is skipped.
//
// It exists so a pristine clone can be shipped as the repository alone and the
// worktree materialized in the guest, halving the bytes that cross the
// transport. It is only correct for a source directory whose worktree is
// exactly HEAD — a dirty tree would lose its uncommitted files.
func GitDirOnlyFilter() func(relPath string, isDir bool) bool {
	return func(relPath string, isDir bool) bool {
		if relPath == "." || relPath == "" {
			return false
		}
		// filepath.Rel yields OS separators; normalize before comparing.
		rel := filepath.ToSlash(relPath)
		return rel != ".git" && !strings.HasPrefix(rel, ".git/")
	}
}
