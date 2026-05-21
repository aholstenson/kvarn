package transfer

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/cockroachdb/errors"
)

const maxBatchSize = 4 * 1024 * 1024 // 4MB per UploadFiles call

// BatchTransferer uploads files in batches to avoid exceeding message size limits.
type BatchTransferer struct {
	// SkipFile, if non-nil, is called for each file and directory encountered
	// during the walk. If it returns true for a directory, the entire subtree
	// is skipped. If it returns true for a file, that file is not transferred.
	SkipFile func(relPath string, isDir bool) bool

	// OnProgress, if non-nil, is called after each batch upload with
	// cumulative bytes sent and total bytes to transfer.
	OnProgress func(bytesSent, totalBytes int64)
}

func (t *BatchTransferer) Upload(ctx context.Context, uploader Uploader, localDir string, remoteDir string) error {
	// Compute total size for progress reporting.
	var totalBytes int64
	if t.OnProgress != nil {
		filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relPath, err := filepath.Rel(localDir, path)
			if err != nil {
				return nil
			}
			if t.SkipFile != nil && t.SkipFile(relPath, d.IsDir()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			totalBytes += info.Size()
			return nil
		})
	}

	var batch []*v1.FileContent
	var batchSize int
	var bytesSent int64

	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return errors.Wrap(err, "rel path")
		}

		if t.SkipFile != nil && t.SkipFile(relPath, d.IsDir()) {
			if d.IsDir() {
				slog.Debug("skipping directory", "path", relPath)
				return fs.SkipDir
			}
			slog.Debug("skipping file", "path", relPath)
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Transfer symlinks as symlinks, preserving the link target.
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return errors.Wrapf(err, "readlink %s", relPath)
			}

			slog.Debug("transferring symlink", "path", relPath, "target", target)
			batch = append(batch, &v1.FileContent{
				Path:          relPath,
				SymlinkTarget: target,
			})
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return errors.Wrapf(err, "read %s", relPath)
		}

		info, err := d.Info()
		if err != nil {
			return errors.Wrapf(err, "stat %s", relPath)
		}

		fileSize := len(content)

		// Flush current batch if adding this file would exceed the limit.
		if batchSize > 0 && batchSize+fileSize > maxBatchSize {
			if err := sendBatch(ctx, uploader, remoteDir, batch); err != nil {
				return err
			}
			bytesSent += int64(batchSize)
			if t.OnProgress != nil {
				t.OnProgress(bytesSent, totalBytes)
			}
			batch = nil
			batchSize = 0
		}

		slog.Debug("transferring file", "path", relPath, "size", fileSize)
		batch = append(batch, &v1.FileContent{
			Path:    relPath,
			Content: content,
			Mode:    uint32(info.Mode().Perm()),
		})
		batchSize += fileSize

		return nil
	})
	if err != nil {
		return err
	}

	// Flush remaining files.
	if len(batch) > 0 {
		if err := sendBatch(ctx, uploader, remoteDir, batch); err != nil {
			return err
		}
		bytesSent += int64(batchSize)
		if t.OnProgress != nil {
			t.OnProgress(bytesSent, totalBytes)
		}
	}
	return nil
}

func sendBatch(ctx context.Context, uploader Uploader, remoteDir string, files []*v1.FileContent) error {
	_, err := uploader.UploadFiles(ctx, &v1.UploadFilesRequest{
		WorkingDir: remoteDir,
		Files:      files,
	})
	if err != nil {
		return errors.Wrap(err, "upload batch")
	}
	return nil
}
