package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
)

// RestoreCache uploads cached tarballs to the guest and extracts them.
func RestoreCache(ctx context.Context, proxy RunnerProxy, provider cache.Provider, projectID string, guestPaths []string, onEvent func(Event)) error {
	total := len(guestPaths)
	for i, guestPath := range guestPaths {
		if onEvent != nil {
			onEvent(CacheProgressEvent{Path: guestPath, Index: i + 1, Total: total, Restoring: true})
		}
		rc, err := provider.Restore(projectID, guestPath)
		if err != nil {
			slog.Warn("failed to restore cache", "path", guestPath, "error", err)
			continue
		}
		if rc == nil {
			continue
		}

		flat := flattenPath(guestPath)
		tmpDir := "/var/tmp/kvarn-cache"
		tmpFile := fmt.Sprintf("%s/%s.tar.zst", tmpDir, flat)

		// Ensure the temp directory exists before streaming.
		proxy.Exec(ctx, &v1.ExecRequest{
			Command:    "mkdir",
			Args:       []string{"-p", tmpDir},
			Privileged: true,
		})

		// Stream tarball to guest. The handler closes the reader when done.
		if err := proxy.StreamToGuest(ctx, tmpFile, rc, 0); err != nil {
			slog.Warn("failed to stream cache tarball", "path", guestPath, "error", err)
			continue
		}

		// Extract tarball with correct ownership and clean up.
		// chown ensures the directory created by mkdir is owned by kvarn so
		// non-privileged processes (like Go's build cache) can write into it.
		script := fmt.Sprintf("mkdir -p %s && chown kvarn:kvarn %s && tar -C %s --zstd --owner=kvarn --group=kvarn -xf %s && rm %s",
			guestPath, guestPath, guestPath, tmpFile, tmpFile)
		resp, err := proxy.Exec(ctx, &v1.ExecRequest{
			Command:    "sh",
			Args:       []string{"-c", script},
			Privileged: true,
		})
		if err != nil {
			slog.Warn("failed to extract cache tarball", "path", guestPath, "error", err)
			continue
		}
		if resp.ExitCode != 0 {
			slog.Warn("cache extract failed", "path", guestPath, "exit_code", resp.ExitCode, "stderr", resp.Stderr)
			continue
		}

		slog.Info("restored cache", "path", guestPath)
	}
	if onEvent != nil {
		onEvent(CacheRestoredEvent{})
	}
	return nil
}

// SaveCache creates tarballs from guest paths and stores them via the provider.
func SaveCache(ctx context.Context, proxy RunnerProxy, provider cache.Provider, projectID string, guestPaths []string, onEvent func(Event)) error {
	var errs []error
	total := len(guestPaths)
	for i, guestPath := range guestPaths {
		if onEvent != nil {
			onEvent(CacheProgressEvent{Path: guestPath, Index: i + 1, Total: total, Restoring: false})
		}
		if err := saveCachePath(ctx, proxy, provider, projectID, guestPath); err != nil {
			slog.Warn("failed to save cache", "path", guestPath, "error", err)
			errs = append(errs, fmt.Errorf("save cache %s: %w", guestPath, err))
		}
	}
	if onEvent != nil {
		onEvent(CacheSavedEvent{})
	}
	return errors.Join(errs...)
}

func saveCachePath(ctx context.Context, proxy RunnerProxy, provider cache.Provider, projectID string, guestPath string) error {
	// Check if the directory exists before trying to tar it.
	resp, err := proxy.Exec(ctx, &v1.ExecRequest{
		Command:    "test",
		Args:       []string{"-d", guestPath},
		Privileged: true,
	})
	if err != nil {
		return fmt.Errorf("check cache dir: %w", err)
	}
	if resp.ExitCode != 0 {
		slog.Debug("cache directory does not exist, skipping", "path", guestPath)
		return nil
	}

	flat := flattenPath(guestPath)
	tmpDir := "/var/tmp/kvarn-cache"
	tmpFile := fmt.Sprintf("%s/%s.tar.zst", tmpDir, flat)

	// Create tarball in guest. Use /var/tmp/kvarn-cache/ instead of /tmp/
	// because /tmp/ may be restricted in some VM configurations.
	script := fmt.Sprintf("mkdir -p %s && tar -C %s --zstd -cf %s .", tmpDir, guestPath, tmpFile)
	resp, err = proxy.Exec(ctx, &v1.ExecRequest{
		Command:    "sh",
		Args:       []string{"-c", script},
		Privileged: true,
	})
	if err != nil {
		return fmt.Errorf("create tarball: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("tar failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}

	// Stream tarball from guest directly into the cache provider.
	pr, pw := io.Pipe()

	// Save runs in a goroutine, consuming from the pipe reader.
	saveErrCh := make(chan error, 1)
	go func() {
		saveErrCh <- provider.Save(projectID, guestPath, pr)
		pr.Close()
	}()

	// Stream file from guest into the pipe writer.
	if err := proxy.StreamFromGuest(ctx, tmpFile, pw); err != nil {
		pw.Close()
		return fmt.Errorf("stream tarball from guest: %w", err)
	}
	pw.Close()

	if err := <-saveErrCh; err != nil {
		return fmt.Errorf("store tarball: %w", err)
	}

	// Clean up temp file in guest.
	proxy.Exec(ctx, &v1.ExecRequest{
		Command:    "rm",
		Args:       []string{"-f", tmpFile},
		Privileged: true,
	})

	slog.Info("saved cache", "path", guestPath)
	return nil
}

// flattenPath converts an absolute path into a flat name by replacing slashes
// with underscores, e.g. "/home/kvarn/go/pkg/mod" → "_home_kvarn_go_pkg_mod"
func flattenPath(p string) string {
	return strings.ReplaceAll(p, "/", "_")
}
