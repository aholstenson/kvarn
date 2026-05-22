package atomicfile_test

import (
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/config/atomicfile"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Write", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("creates a new file with the requested content and mode", func() {
		path := filepath.Join(dir, "file.toml")
		Expect(atomicfile.Write(path, []byte("hello"), 0o600)).To(Succeed())

		data, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("hello"))

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("honours a different mode", func() {
		path := filepath.Join(dir, "file.toml")
		Expect(atomicfile.Write(path, []byte("x"), 0o644)).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o644)))
	})

	It("overwrites an existing file with the new content", func() {
		path := filepath.Join(dir, "file.toml")
		Expect(atomicfile.Write(path, []byte("first"), 0o600)).To(Succeed())
		Expect(atomicfile.Write(path, []byte("second"), 0o600)).To(Succeed())

		data, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("second"))
	})

	It("leaves no temp files behind", func() {
		path := filepath.Join(dir, "file.toml")
		Expect(atomicfile.Write(path, []byte("data"), 0o600)).To(Succeed())

		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].Name()).To(Equal("file.toml"))
	})
})
