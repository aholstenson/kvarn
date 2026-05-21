package transfer_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/klauspost/compress/zstd"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox/transfer"
)

// mockUploader records UploadFiles, StreamToGuest, and Exec calls.
type mockUploader struct {
	mu    sync.Mutex
	calls []*v1.UploadFilesRequest

	streamData bytes.Buffer
	streamDest string
	execCalls  []*v1.ExecRequest
}

func (m *mockUploader) UploadFiles(_ context.Context, req *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	var count int32
	for range req.Files {
		count++
	}
	return &v1.UploadFilesResponse{FilesWritten: count}, nil
}

func (m *mockUploader) StreamToGuest(_ context.Context, destPath string, src io.Reader, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamDest = destPath
	_, err := io.Copy(&m.streamData, src)
	return err
}

func (m *mockUploader) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCalls = append(m.execCalls, req)
	return &v1.ExecResponse{ExitCode: 0}, nil
}

func (m *mockUploader) allFiles() []*v1.FileContent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var files []*v1.FileContent
	for _, call := range m.calls {
		files = append(files, call.Files...)
	}
	return files
}

// extractTar decompresses zstd and reads tar entries into a map.
func (m *mockUploader) extractTar() map[string]tarEntry {
	m.mu.Lock()
	data := m.streamData.Bytes()
	m.mu.Unlock()

	zr, err := zstd.NewReader(bytes.NewReader(data))
	Expect(err).NotTo(HaveOccurred())
	defer zr.Close()

	tr := tar.NewReader(zr)
	entries := make(map[string]tarEntry)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		Expect(err).NotTo(HaveOccurred())

		entry := tarEntry{header: hdr}
		if hdr.Typeflag == tar.TypeReg {
			var buf bytes.Buffer
			_, err := io.Copy(&buf, tr)
			Expect(err).NotTo(HaveOccurred())
			entry.content = buf.Bytes()
		}
		entries[hdr.Name] = entry
	}
	return entries
}

type tarEntry struct {
	header  *tar.Header
	content []byte
}

var _ = Describe("BatchTransferer", func() {
	var (
		t      *transfer.BatchTransferer
		mock   *mockUploader
		tmpDir string
		ctx    context.Context
	)

	BeforeEach(func() {
		t = &transfer.BatchTransferer{}
		mock = &mockUploader{}
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "transfer-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("uploads files from a directory", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("world"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		Expect(files).To(HaveLen(2))

		paths := []string{}
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		Expect(paths).To(ContainElements("a.txt", "b.txt"))
	})

	It("uploads nested directory structure", func() {
		subDir := filepath.Join(tmpDir, "sub", "dir")
		Expect(os.MkdirAll(subDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "root.txt"), []byte("root"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		Expect(files).To(HaveLen(2))

		paths := []string{}
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		Expect(paths).To(ContainElement("root.txt"))
		Expect(paths).To(ContainElement(filepath.Join("sub", "dir", "nested.txt")))
	})

	It("uploads .git directory contents", func() {
		gitDir := filepath.Join(tmpDir, ".git")
		Expect(os.MkdirAll(gitDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("content"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		paths := []string{}
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		Expect(paths).To(ContainElement(filepath.Join(".git", "HEAD")))
	})

	It("preserves file permissions", func() {
		execPath := filepath.Join(tmpDir, "exec.sh")
		Expect(os.WriteFile(execPath, []byte("#!/bin/sh"), 0644)).To(Succeed())
		Expect(os.Chmod(execPath, 0755)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		Expect(files).To(HaveLen(1))
		Expect(files[0].Mode).To(Equal(uint32(0755)))
	})

	It("sets working dir on upload requests", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("x"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/remote/path")
		Expect(err).NotTo(HaveOccurred())

		mock.mu.Lock()
		defer mock.mu.Unlock()
		Expect(mock.calls).To(HaveLen(1))
		Expect(mock.calls[0].WorkingDir).To(Equal("/remote/path"))
	})

	It("handles empty directory", func() {
		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		mock.mu.Lock()
		defer mock.mu.Unlock()
		Expect(mock.calls).To(BeEmpty())
	})

	It("transfers symlinks to files as symlinks", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "real.txt"), []byte("content"), 0644)).To(Succeed())
		Expect(os.Symlink("real.txt", filepath.Join(tmpDir, "link.txt"))).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		Expect(files).To(HaveLen(2))

		var realFile, linkFile *v1.FileContent
		for _, f := range files {
			switch f.Path {
			case "real.txt":
				realFile = f
			case "link.txt":
				linkFile = f
			}
		}

		Expect(realFile).NotTo(BeNil())
		Expect(realFile.Content).To(Equal([]byte("content")))
		Expect(realFile.SymlinkTarget).To(BeEmpty())

		Expect(linkFile).NotTo(BeNil())
		Expect(linkFile.SymlinkTarget).To(Equal("real.txt"))
		Expect(linkFile.Content).To(BeEmpty())
	})

	It("skips files when SkipFile returns true", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("world"), 0644)).To(Succeed())

		t.SkipFile = func(relPath string, isDir bool) bool {
			return relPath == "b.txt"
		}

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		Expect(files).To(HaveLen(1))
		Expect(files[0].Path).To(Equal("a.txt"))
	})

	It("skips entire directories when SkipFile returns true for a dir", func() {
		subDir := filepath.Join(tmpDir, "skip-me")
		Expect(os.MkdirAll(subDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "keep.txt"), []byte("keep"), 0644)).To(Succeed())

		t.SkipFile = func(relPath string, isDir bool) bool {
			return relPath == "skip-me" && isDir
		}

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		Expect(files).To(HaveLen(1))
		Expect(files[0].Path).To(Equal("keep.txt"))
	})

	It("transfers symlinks to directories as symlinks", func() {
		subDir := filepath.Join(tmpDir, "realdir")
		Expect(os.MkdirAll(subDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("inside"), 0644)).To(Succeed())
		Expect(os.Symlink("realdir", filepath.Join(tmpDir, "linkdir"))).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		files := mock.allFiles()
		paths := []string{}
		for _, f := range files {
			paths = append(paths, f.Path)
		}

		// The real file and the symlink should be transferred, but not the contents via the symlink.
		Expect(paths).To(ContainElement(filepath.Join("realdir", "file.txt")))
		Expect(paths).To(ContainElement("linkdir"))
		Expect(paths).NotTo(ContainElement(filepath.Join("linkdir", "file.txt")))

		// Verify the symlink entry
		for _, f := range files {
			if f.Path == "linkdir" {
				Expect(f.SymlinkTarget).To(Equal("realdir"))
			}
		}
	})
})

var _ = Describe("StreamingTransferer", func() {
	var (
		t      *transfer.StreamingTransferer
		mock   *mockUploader
		tmpDir string
		ctx    context.Context
	)

	BeforeEach(func() {
		t = &transfer.StreamingTransferer{}
		mock = &mockUploader{}
		ctx = context.Background()
		var err error
		tmpDir, err = os.MkdirTemp("", "streaming-test-*")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		os.RemoveAll(tmpDir)
	})

	It("uploads files from a directory", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("world"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		entries := mock.extractTar()
		Expect(entries).To(HaveKey("a.txt"))
		Expect(entries).To(HaveKey("b.txt"))
		Expect(entries["a.txt"].content).To(Equal([]byte("hello")))
		Expect(entries["b.txt"].content).To(Equal([]byte("world")))
	})

	It("uploads nested directory structure", func() {
		subDir := filepath.Join(tmpDir, "sub", "dir")
		Expect(os.MkdirAll(subDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "root.txt"), []byte("root"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		entries := mock.extractTar()
		Expect(entries).To(HaveKey("sub/"))
		Expect(entries).To(HaveKey("sub/dir/"))
		Expect(entries).To(HaveKey("sub/dir/nested.txt"))
		Expect(entries).To(HaveKey("root.txt"))
		Expect(entries["sub/dir/nested.txt"].content).To(Equal([]byte("nested")))
	})

	It("preserves file permissions", func() {
		execPath := filepath.Join(tmpDir, "exec.sh")
		Expect(os.WriteFile(execPath, []byte("#!/bin/sh"), 0644)).To(Succeed())
		Expect(os.Chmod(execPath, 0755)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		entries := mock.extractTar()
		Expect(entries).To(HaveKey("exec.sh"))
		Expect(entries["exec.sh"].header.Mode).To(Equal(int64(0755)))
	})

	It("transfers symlinks", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "real.txt"), []byte("content"), 0644)).To(Succeed())
		Expect(os.Symlink("real.txt", filepath.Join(tmpDir, "link.txt"))).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		entries := mock.extractTar()
		Expect(entries).To(HaveKey("real.txt"))
		Expect(entries).To(HaveKey("link.txt"))
		Expect(entries["link.txt"].header.Typeflag).To(Equal(byte(tar.TypeSymlink)))
		Expect(entries["link.txt"].header.Linkname).To(Equal("real.txt"))
	})

	It("skips files when SkipFile returns true", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("world"), 0644)).To(Succeed())

		t.SkipFile = func(relPath string, isDir bool) bool {
			return relPath == "b.txt"
		}

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		entries := mock.extractTar()
		Expect(entries).To(HaveKey("a.txt"))
		Expect(entries).NotTo(HaveKey("b.txt"))
	})

	It("skips entire directories when SkipFile returns true for a dir", func() {
		subDir := filepath.Join(tmpDir, "skip-me")
		Expect(os.MkdirAll(subDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("nested"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "keep.txt"), []byte("keep"), 0644)).To(Succeed())

		t.SkipFile = func(relPath string, isDir bool) bool {
			return relPath == "skip-me" && isDir
		}

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		entries := mock.extractTar()
		Expect(entries).To(HaveKey("keep.txt"))
		Expect(entries).NotTo(HaveKey("skip-me/"))
		Expect(entries).NotTo(HaveKey("skip-me/nested.txt"))
	})

	It("reports progress", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0644)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("world!"), 0644)).To(Succeed())

		var progressCalls []int64
		t.OnProgress = func(sent, total int64) {
			progressCalls = append(progressCalls, sent)
		}

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		// Should have been called at least once per file.
		Expect(len(progressCalls)).To(BeNumerically(">=", 2))
		// Final call should equal total bytes.
		Expect(progressCalls[len(progressCalls)-1]).To(Equal(int64(11)))
	})

	It("calls mkdir and extract via Exec", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "f.txt"), []byte("x"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		mock.mu.Lock()
		defer mock.mu.Unlock()
		// Two Exec calls: mkdir -p for tmp dir, and tar extract.
		Expect(mock.execCalls).To(HaveLen(2))
		Expect(mock.execCalls[0].Command).To(Equal("mkdir"))
		Expect(mock.execCalls[1].Command).To(Equal("sh"))
	})

	It("streams to the correct destination path", func() {
		Expect(os.WriteFile(filepath.Join(tmpDir, "f.txt"), []byte("x"), 0644)).To(Succeed())

		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		Expect(mock.streamDest).To(Equal("/var/tmp/kvarn-transfer/source.tar.zst"))
	})

	It("handles empty directory", func() {
		err := t.Upload(ctx, mock, tmpDir, "/home/kvarn/workspace")
		Expect(err).NotTo(HaveOccurred())

		// Should still create tmp dir and call extract, even if tarball is empty.
		mock.mu.Lock()
		defer mock.mu.Unlock()
		Expect(mock.execCalls).To(HaveLen(2))
	})
})
