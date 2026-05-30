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

// kvarnHome is the home directory of the unprivileged user that runs jobs.
// Cache directories under it must end up owned by kvarn so the job can write
// into them and their parents.
const kvarnHome = "/home/kvarn"

// RestoreCache uploads cached tarballs to the guest and extracts them.
func RestoreCache(ctx context.Context, proxy RunnerProxy, provider cache.Provider, layers []cache.Layer, onEvent func(Event)) error {
	total := len(layers)
	for i, layer := range layers {
		if onEvent != nil {
			onEvent(CacheProgressEvent{Path: layer.GuestPath, Index: i + 1, Total: total, Restoring: true})
		}
		res, err := provider.Restore(layer.Key)
		if err != nil {
			slog.Warn("failed to restore cache", "path", layer.GuestPath, "error", err)
			continue
		}
		if res == nil {
			continue
		}

		if res.Warm {
			slog.Info("cache warm start", "bucket", layer.Key.Bucket, "path", layer.GuestPath,
				"requested", layer.Key.InputKey, "served", res.InputKey)
		}

		guestPath := layer.GuestPath
		tmpDir := "/var/tmp/kvarn-cache"
		tmpFile := fmt.Sprintf("%s/%s.tar.zst", tmpDir, tempName(layer.Key))

		// Ensure the temp directory exists before streaming.
		proxy.Exec(ctx, &v1.ExecRequest{
			Command:    "mkdir",
			Args:       []string{"-p", tmpDir},
			Privileged: true,
		})

		// Stream tarball to guest. The handler closes the reader when done.
		if err := proxy.StreamToGuest(ctx, tmpFile, res.Reader, 0); err != nil {
			res.Reader.Close()
			slog.Warn("failed to stream cache tarball", "path", guestPath, "error", err)
			continue
		}

		// Extract tarball with correct ownership and clean up. The privileged
		// mkdir -p creates any missing ancestors as root, so chown every
		// directory in the chain below the kvarn home (not just the leaf):
		// otherwise caching a nested path like /home/kvarn/.cache/nix would
		// leave /home/kvarn/.cache root-owned, and the job's own writes there
		// (e.g. Go creating .cache/go-build) would be denied.
		chownTargets := strings.Join(ownedDirs(guestPath), " ")
		script := fmt.Sprintf("mkdir -p %s && chown kvarn:kvarn %s && tar -C %s --zstd --owner=kvarn --group=kvarn -xf %s && rm %s",
			guestPath, chownTargets, guestPath, tmpFile, tmpFile)
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

		slog.Info("restored cache", "path", guestPath, "bucket", layer.Key.Bucket, "warm", res.Warm)
	}
	if onEvent != nil {
		onEvent(CacheRestoredEvent{})
	}
	return nil
}

// SaveCache creates tarballs from guest paths and stores them via the provider.
// Write-once: a layer already present is skipped before any guest-side tar runs.
func SaveCache(ctx context.Context, proxy RunnerProxy, provider cache.Provider, layers []cache.Layer, onEvent func(Event)) error {
	var errs []error
	total := len(layers)
	for i, layer := range layers {
		if onEvent != nil {
			onEvent(CacheProgressEvent{Path: layer.GuestPath, Index: i + 1, Total: total, Restoring: false})
		}
		if has, err := provider.Has(layer.Key); err == nil && has {
			slog.Debug("cache already present, skipping save", "path", layer.GuestPath, "bucket", layer.Key.Bucket)
			continue
		}
		if err := saveCacheLayer(ctx, proxy, provider, layer); err != nil {
			slog.Warn("failed to save cache", "path", layer.GuestPath, "error", err)
			errs = append(errs, fmt.Errorf("save cache %s: %w", layer.GuestPath, err))
		}
	}
	if onEvent != nil {
		onEvent(CacheSavedEvent{})
	}
	return errors.Join(errs...)
}

func saveCacheLayer(ctx context.Context, proxy RunnerProxy, provider cache.Provider, layer cache.Layer) error {
	guestPath := layer.GuestPath

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

	tmpDir := "/var/tmp/kvarn-cache"
	tmpFile := fmt.Sprintf("%s/%s.tar.zst", tmpDir, tempName(layer.Key))

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
	saveErrCh := make(chan error, 1)
	go func() {
		saveErrCh <- provider.Save(layer.Key, pr)
		pr.Close()
	}()

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

	slog.Info("saved cache", "path", guestPath, "bucket", layer.Key.Bucket)
	return nil
}

// tempName builds a collision-free guest temp-file stem for a layer. The
// InputKey alone can collide across buckets (two tools sharing a lockfile
// digest), so the bucket is folded in. Bucket may contain ':' or '/', which
// are unsafe in a filename, so they are flattened to '_'.
func tempName(key cache.Key) string {
	bucket := key.Bucket
	out := make([]rune, 0, len(bucket))
	for _, r := range bucket {
		if r == '/' || r == ':' || r == ' ' {
			out = append(out, '_')
			continue
		}
		out = append(out, r)
	}
	return string(out) + "-" + key.InputKey
}

// ownedDirs returns the directory chain that must be chowned to the kvarn user
// after a privileged mkdir -p of guestPath: every path component below the
// kvarn home directory, down to and including guestPath. This fixes ancestors
// that mkdir -p created as root (e.g. /home/kvarn/.cache when caching
// /home/kvarn/.cache/nix). For paths outside the home directory only the leaf
// is returned — the caller cannot assume the kvarn user owns arbitrary system
// directories, matching the prior leaf-only behavior for such paths.
func ownedDirs(guestPath string) []string {
	if guestPath == kvarnHome || !strings.HasPrefix(guestPath, kvarnHome+"/") {
		return []string{guestPath}
	}
	rel := strings.TrimPrefix(guestPath, kvarnHome+"/")
	var dirs []string
	cur := kvarnHome
	for _, p := range strings.Split(rel, "/") {
		if p == "" {
			continue
		}
		cur += "/" + p
		dirs = append(dirs, cur)
	}
	return dirs
}
