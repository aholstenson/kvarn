package disk_test

import (
	"encoding/binary"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/disk"
)

// qcow2Header builds the fixed 32-byte prefix of a qcow2 image. Only the
// fields Qcow2VirtualSize reads are meaningful.
func qcow2Header(version uint32, virtualSize uint64) []byte {
	h := make([]byte, 32)
	copy(h[0:4], []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(h[4:8], version)
	binary.BigEndian.PutUint64(h[24:32], virtualSize)
	return h
}

var _ = Describe("Qcow2VirtualSize", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, data, 0o600)).To(Succeed())
		return path
	}

	It("reads the virtual size of a v3 image", func() {
		path := write("v3.qcow2", qcow2Header(3, 3*1024*1024*1024))
		Expect(disk.Qcow2VirtualSize(path)).To(Equal(int64(3 * 1024 * 1024 * 1024)))
	})

	It("reads the virtual size of a v2 image", func() {
		path := write("v2.qcow2", qcow2Header(2, 512*1024*1024))
		Expect(disk.Qcow2VirtualSize(path)).To(Equal(int64(512 * 1024 * 1024)))
	})

	It("rejects a file that is not qcow2", func() {
		path := write("raw.img", make([]byte, 64))
		_, err := disk.Qcow2VirtualSize(path)
		Expect(err).To(MatchError(ContainSubstring("not a qcow2 image")))
	})

	It("rejects an unsupported qcow2 version", func() {
		path := write("v4.qcow2", qcow2Header(4, 1024))
		_, err := disk.Qcow2VirtualSize(path)
		Expect(err).To(MatchError(ContainSubstring("unsupported qcow2 version 4")))
	})

	It("rejects a file too short to hold a header", func() {
		path := write("short.qcow2", qcow2Header(3, 1024)[:16])
		_, err := disk.Qcow2VirtualSize(path)
		Expect(err).To(HaveOccurred())
	})

	It("reports a missing file", func() {
		_, err := disk.Qcow2VirtualSize(filepath.Join(dir, "missing.qcow2"))
		Expect(err).To(MatchError(os.ErrNotExist))
	})
})
