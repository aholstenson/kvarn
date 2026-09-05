//go:build darwin

package disk_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/disk"
)

var _ = Describe("CloneFile", func() {
	var dir, src string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		src = filepath.Join(dir, "source.img")
		Expect(os.WriteFile(src, []byte("disk contents"), 0o644)).To(Succeed())
	})

	It("copies the contents of the source", func() {
		dst := filepath.Join(dir, "clone.img")
		Expect(disk.CloneFile(src, dst)).To(Succeed())
		Expect(os.ReadFile(dst)).To(Equal([]byte("disk contents")))
	})

	It("gives the clone its own data", func() {
		dst := filepath.Join(dir, "clone.img")
		Expect(disk.CloneFile(src, dst)).To(Succeed())

		Expect(os.WriteFile(dst, []byte("written by the VM"), 0o644)).To(Succeed())
		Expect(os.Remove(src)).To(Succeed())

		Expect(os.ReadFile(dst)).To(Equal([]byte("written by the VM")))
	})

	It("refuses to overwrite an existing file", func() {
		dst := filepath.Join(dir, "clone.img")
		Expect(os.WriteFile(dst, []byte("keep"), 0o644)).To(Succeed())

		Expect(disk.CloneFile(src, dst)).NotTo(Succeed())
		Expect(os.ReadFile(dst)).To(Equal([]byte("keep")))
	})

	It("errors when the source does not exist", func() {
		err := disk.CloneFile(filepath.Join(dir, "missing.img"), filepath.Join(dir, "clone.img"))
		Expect(err).To(HaveOccurred())
	})
})
