package transfer

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/klauspost/compress/zstd"
)

const transferTmpDir = "/var/tmp/kvarn-transfer"

// StreamingTransferer uploads a directory as a zstd-compressed tarball streamed
// to the guest via StreamToGuest, then extracts it remotely.
type StreamingTransferer struct {
	// SkipFile, if non-nil, is called for each file and directory encountered
	// during the walk. If it returns true for a directory, the entire subtree
	// is skipped. If it returns true for a file, that file is not transferred.
	SkipFile func(relPath string, isDir bool) bool

	// OnProgress, if non-nil, is called periodically with cumulative bytes
	// written to the tar stream and total bytes to transfer.
	OnProgress func(bytesSent, totalBytes int64)
}

func (t *StreamingTransferer) Upload(ctx context.Context, uploader Uploader, localDir string, remoteDir string) error {
	// Compute total size for progress reporting.
	var totalBytes int64
	err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
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
	if err != nil {
		return fmt.Errorf("compute transfer size: %w", err)
	}

	// Ensure temp directory exists on the guest.
	tmpArchive := transferTmpDir + "/source.tar.zst"
	_, err = uploader.Exec(ctx, &v1.ExecRequest{
		Command: "mkdir",
		Args:    []string{"-p", transferTmpDir},
	})
	if err != nil {
		return fmt.Errorf("create transfer tmp dir: %w", err)
	}

	// Set up pipe: tar+zstd goroutine writes to pw, StreamToGuest reads from pr.
	pr, pw := io.Pipe()

	var bytesSent atomic.Int64
	var tarErr error

	done := make(chan struct{})
	go func() {
		defer close(done)
		tarErr = t.writeTarball(pw, localDir, &bytesSent, totalBytes)
		// Close pipe writer — propagates error or EOF to reader side.
		if tarErr != nil {
			pw.CloseWithError(tarErr)
		} else {
			pw.Close()
		}
	}()

	// Stream the archive to the guest. Size 0 means unknown/streaming.
	streamErr := uploader.StreamToGuest(ctx, tmpArchive, pr, 0)

	// Wait for the tar goroutine to finish.
	<-done

	if tarErr != nil {
		return fmt.Errorf("create tarball: %w", tarErr)
	}
	if streamErr != nil {
		return fmt.Errorf("stream to guest: %w", streamErr)
	}

	// Extract on guest.
	resp, err := uploader.Exec(ctx, &v1.ExecRequest{
		Command: "sh",
		Args: []string{
			"-c",
			fmt.Sprintf(
				"mkdir -p %s && zstd -d -c %s | tar -C %s --owner=kvarn --group=kvarn -xf - && rm %s",
				remoteDir, tmpArchive, remoteDir, tmpArchive,
			),
		},
	})
	if err != nil {
		return fmt.Errorf("extract tarball on guest: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("extract failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}

	return nil
}

// writeTarball walks localDir and writes a zstd-compressed tar to w.
func (t *StreamingTransferer) writeTarball(w io.Writer, localDir string, bytesSent *atomic.Int64, totalBytes int64) error {
	zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return fmt.Errorf("create zstd encoder: %w", err)
	}
	defer zw.Close()

	tw := tar.NewWriter(zw)
	defer tw.Close()

	err = filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(localDir, path)
		if err != nil {
			return fmt.Errorf("rel path: %w", err)
		}

		if t.SkipFile != nil && t.SkipFile(relPath, d.IsDir()) {
			if d.IsDir() {
				slog.Debug("skipping directory", "path", relPath)
				return fs.SkipDir
			}
			slog.Debug("skipping file", "path", relPath)
			return nil
		}

		// Skip the root directory entry itself.
		if relPath == "." {
			return nil
		}

		// Handle symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", relPath, err)
			}
			slog.Debug("transferring symlink", "path", relPath, "target", target)
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     relPath,
				Linkname: target,
			})
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", relPath, err)
		}

		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     relPath + "/",
				Mode:     int64(info.Mode().Perm()),
			})
		}

		// Regular file.
		slog.Debug("transferring file", "path", relPath, "size", info.Size())
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     relPath,
			Size:     info.Size(),
			Mode:     int64(info.Mode().Perm()),
		}); err != nil {
			return fmt.Errorf("write header %s: %w", relPath, err)
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", relPath, err)
		}
		defer f.Close()

		cr := &countingReader{
			r:          f,
			bytesSent:  bytesSent,
			totalBytes: totalBytes,
			onProgress: t.OnProgress,
		}
		if _, err := io.Copy(tw, cr); err != nil {
			return fmt.Errorf("write %s: %w", relPath, err)
		}

		return nil
	})

	if err != nil {
		return err
	}

	// Close tar then zstd to flush all data.
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close zstd writer: %w", err)
	}

	return nil
}

// countingReader wraps a reader and reports progress after each Read call.
type countingReader struct {
	r          io.Reader
	bytesSent  *atomic.Int64
	totalBytes int64
	onProgress func(bytesSent, totalBytes int64)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		sent := cr.bytesSent.Add(int64(n))
		if cr.onProgress != nil {
			cr.onProgress(sent, cr.totalBytes)
		}
	}
	return n, err
}
