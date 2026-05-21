package cache_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/sandbox/cache"
)

var _ = Describe("FileCache", func() {
	var fc *cache.FileCache

	BeforeEach(func() {
		fc = &cache.FileCache{BaseDir: GinkgoT().TempDir()}
	})

	Describe("Save and Restore", func() {
		It("round-trips data", func() {
			data := []byte("tarball-contents")
			Expect(fc.Save("proj1", "/home/kvarn/.cache/go", bytes.NewReader(data))).To(Succeed())

			rc, err := fc.Restore("proj1", "/home/kvarn/.cache/go")
			Expect(err).NotTo(HaveOccurred())
			Expect(rc).NotTo(BeNil())
			defer rc.Close()

			got, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(data))
		})

		It("returns nil for non-existent cache", func() {
			rc, err := fc.Restore("proj1", "/home/kvarn/.cache/go")
			Expect(err).NotTo(HaveOccurred())
			Expect(rc).To(BeNil())
		})

		It("overwrites existing cache", func() {
			Expect(fc.Save("proj1", "/path", bytes.NewReader([]byte("old")))).To(Succeed())
			Expect(fc.Save("proj1", "/path", bytes.NewReader([]byte("new")))).To(Succeed())

			rc, err := fc.Restore("proj1", "/path")
			Expect(err).NotTo(HaveOccurred())
			defer rc.Close()

			got, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]byte("new")))
		})

		It("creates a SOURCE metadata file", func() {
			Expect(fc.Save("proj1", "/path", bytes.NewReader([]byte("data")))).To(Succeed())

			content, err := os.ReadFile(filepath.Join(fc.BaseDir, "proj1", "SOURCE"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("proj1\n"))
		})

		It("stores files as .tar.zst", func() {
			Expect(fc.Save("proj1", "/home/kvarn/go", bytes.NewReader([]byte("data")))).To(Succeed())

			_, err := os.Stat(filepath.Join(fc.BaseDir, "proj1", "_home_kvarn_go.tar.zst"))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Clear", func() {
		It("removes the project cache directory", func() {
			Expect(fc.Save("proj1", "/path", bytes.NewReader([]byte("data")))).To(Succeed())
			Expect(fc.Clear("proj1")).To(Succeed())

			rc, err := fc.Restore("proj1", "/path")
			Expect(err).NotTo(HaveOccurred())
			Expect(rc).To(BeNil())
		})

		It("succeeds for non-existent projects", func() {
			Expect(fc.Clear("nonexistent")).To(Succeed())
		})
	})
})
