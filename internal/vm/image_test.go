package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("EnsureDiskImage resolution order", func() {
	var (
		binDir   string
		cacheDir string
	)

	BeforeEach(func() {
		binDir = GinkgoT().TempDir()
		cacheDir = GinkgoT().TempDir()

		origExec := executable
		origCache := userCacheDir
		// By default point the binary at an empty dir (no local dist/) and the
		// cache at a temp dir; individual specs populate what they need.
		executable = func() (string, error) { return filepath.Join(binDir, "kvarn"), nil }
		userCacheDir = func() (string, error) { return cacheDir, nil }
		DeferCleanup(func() {
			executable = origExec
			userCacheDir = origCache
		})
	})

	It("uses an explicit path when it exists", func() {
		p := filepath.Join(GinkgoT().TempDir(), "custom.qcow2")
		Expect(os.WriteFile(p, []byte("img"), 0o644)).To(Succeed())

		got, err := EnsureDiskImage(context.Background(), DownloadOpts{Path: p})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(p))
	})

	It("errors when the explicit path does not exist", func() {
		_, err := EnsureDiskImage(context.Background(), DownloadOpts{
			Path: filepath.Join(GinkgoT().TempDir(), "missing.qcow2"),
		})
		Expect(err).To(HaveOccurred())
	})

	It("finds a binary-relative dist/ image", func() {
		distDir := filepath.Join(binDir, "dist", runtime.GOARCH)
		Expect(os.MkdirAll(distDir, 0o755)).To(Succeed())
		want := filepath.Join(distDir, "disk.qcow2")
		Expect(os.WriteFile(want, []byte("img"), 0o644)).To(Succeed())

		got, err := EnsureDiskImage(context.Background(), DownloadOpts{})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	})

	It("returns a cached image when no local image exists", func() {
		version := "v9.9.9"
		dir := filepath.Join(cacheDir, "kvarn", "images", version, runtime.GOARCH)
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		want := filepath.Join(dir, cachedImageName)
		Expect(os.WriteFile(want, []byte("img"), 0o644)).To(Succeed())

		got, err := EnsureDiskImage(context.Background(), DownloadOpts{Version: version})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	})

	It("errors with --no-download when nothing is cached", func() {
		_, err := EnsureDiskImage(context.Background(), DownloadOpts{
			Version:    "v9.9.9",
			NoDownload: true,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no-download"))
	})
})

var _ = Describe("downloadDiskImage", func() {
	const (
		version = "v0.1.0"
		arch    = "arm64"
	)
	var (
		cacheDir string
		content  []byte
		sumHex   string
	)

	BeforeEach(func() {
		cacheDir = GinkgoT().TempDir()
		content = []byte("fake qcow2 image contents")
		sum := sha256.Sum256(content)
		sumHex = hex.EncodeToString(sum[:])

		origCache := userCacheDir
		userCacheDir = func() (string, error) { return cacheDir, nil }
		DeferCleanup(func() { userCacheDir = origCache })
	})

	// startServer wires an httptest server that serves the image and a sha256
	// sidecar with the given digest, and points releaseBaseURL at it.
	startServer := func(servedSum string) {
		mux := http.NewServeMux()
		mux.HandleFunc("/"+version+"/kvarn-disk-"+arch+".qcow2", func(w http.ResponseWriter, _ *http.Request) {
			w.Write(content)
		})
		mux.HandleFunc("/"+version+"/kvarn-disk-"+arch+".qcow2.sha256", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "%s  kvarn-disk-%s.qcow2\n", servedSum, arch)
		})
		srv := httptest.NewServer(mux)
		DeferCleanup(srv.Close)

		origBase := releaseBaseURL
		releaseBaseURL = srv.URL
		DeferCleanup(func() { releaseBaseURL = origBase })
	}

	It("downloads and verifies the image into the cache", func() {
		startServer(sumHex)

		got, err := downloadDiskImage(context.Background(), version, arch, nil)
		Expect(err).NotTo(HaveOccurred())

		want := filepath.Join(cacheDir, "kvarn", "images", version, arch, cachedImageName)
		Expect(got).To(Equal(want))

		data, err := os.ReadFile(got)
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(Equal(content))
	})

	It("reports progress while downloading", func() {
		startServer(sumHex)

		var lastDone int64
		_, err := downloadDiskImage(context.Background(), version, arch, func(done, _ int64) {
			lastDone = done
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(lastDone).To(Equal(int64(len(content))))
	})

	It("fails on a checksum mismatch and leaves no image behind", func() {
		startServer(hex.EncodeToString(make([]byte, sha256.Size)))

		_, err := downloadDiskImage(context.Background(), version, arch, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("checksum mismatch"))

		dest := filepath.Join(cacheDir, "kvarn", "images", version, arch, cachedImageName)
		_, statErr := os.Stat(dest)
		Expect(os.IsNotExist(statErr)).To(BeTrue())
	})
})
