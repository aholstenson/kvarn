package vm

import (
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("EnsureRawDiskImage", func() {
	var (
		cacheDir    string
		rawDir      string
		src         string
		conversions int
	)

	// writeSource writes a stand-in qcow2 image, with a modification time far
	// enough back that a rewrite is a visible change.
	writeSource := func(content string, age time.Duration) {
		Expect(os.WriteFile(src, []byte(content), 0o644)).To(Succeed())
		stamp := time.Now().Add(-age)
		Expect(os.Chtimes(src, stamp, stamp)).To(Succeed())
	}

	BeforeEach(func() {
		cacheDir = GinkgoT().TempDir()
		rawDir = filepath.Join(cacheDir, "kvarn", "images", "raw")
		src = filepath.Join(GinkgoT().TempDir(), "disk.qcow2")
		conversions = 0

		origCache := userCacheDir
		origConvert := convertToRaw
		userCacheDir = func() (string, error) { return cacheDir, nil }
		convertToRaw = func(from, to string) error {
			conversions++
			data, err := os.ReadFile(from)
			if err != nil {
				return err
			}
			return os.WriteFile(to, append([]byte("raw:"), data...), 0o644)
		}
		DeferCleanup(func() {
			userCacheDir = origCache
			convertToRaw = origConvert
		})

		writeSource("image-a", time.Hour)
	})

	It("converts the image into the cache", func() {
		got, err := EnsureRawDiskImage(src)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HavePrefix(rawDir))
		Expect(os.ReadFile(got)).To(Equal([]byte("raw:image-a")))
		Expect(conversions).To(Equal(1))
	})

	It("reuses the cached copy on a second call", func() {
		first, err := EnsureRawDiskImage(src)
		Expect(err).NotTo(HaveOccurred())

		second, err := EnsureRawDiskImage(src)
		Expect(err).NotTo(HaveOccurred())
		Expect(second).To(Equal(first))
		Expect(conversions).To(Equal(1))
	})

	It("reconverts and prunes when the image is rebuilt in place", func() {
		first, err := EnsureRawDiskImage(src)
		Expect(err).NotTo(HaveOccurred())

		writeSource("image-b-longer", 0)

		second, err := EnsureRawDiskImage(src)
		Expect(err).NotTo(HaveOccurred())
		Expect(second).NotTo(Equal(first))
		Expect(os.ReadFile(second)).To(Equal([]byte("raw:image-b-longer")))
		Expect(conversions).To(Equal(2))

		_, err = os.Stat(first)
		Expect(os.IsNotExist(err)).To(BeTrue(), "the copy of the previous image should be pruned")
	})

	It("keeps copies of other images", func() {
		first, err := EnsureRawDiskImage(src)
		Expect(err).NotTo(HaveOccurred())

		other := filepath.Join(GinkgoT().TempDir(), "disk.qcow2")
		Expect(os.WriteFile(other, []byte("image-c"), 0o644)).To(Succeed())
		second, err := EnsureRawDiskImage(other)
		Expect(err).NotTo(HaveOccurred())

		Expect(second).NotTo(Equal(first))
		Expect(first).To(BeAnExistingFile())
		Expect(second).To(BeAnExistingFile())
	})

	It("leaves nothing behind when the conversion fails", func() {
		convertToRaw = func(_, _ string) error { return os.ErrInvalid }

		_, err := EnsureRawDiskImage(src)
		Expect(err).To(HaveOccurred())

		entries, err := os.ReadDir(rawDir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})

	It("errors when the image does not exist", func() {
		_, err := EnsureRawDiskImage(filepath.Join(GinkgoT().TempDir(), "missing.qcow2"))
		Expect(err).To(HaveOccurred())
	})
})
