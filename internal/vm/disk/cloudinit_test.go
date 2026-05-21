package disk_test

import (
	"io"
	"os"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/disk"
)

var _ = Describe("CreateCloudInitDisk", func() {
	It("creates a valid cloud-init ISO", func() {
		path := GinkgoT().TempDir() + "/cidata.iso"

		const token = "test-token-abc123"
		const vsockPort = 1024

		Expect(disk.CreateCloudInitDisk(path, disk.CloudInitOpts{Token: token, VsockPort: vsockPort})).To(Succeed())

		// Verify the file was created.
		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Size()).NotTo(BeZero())

		// Open and read back the ISO.
		d, err := diskfs.Open(path)
		Expect(err).NotTo(HaveOccurred())
		defer d.Close()

		fs, err := d.GetFilesystem(0)
		Expect(err).NotTo(HaveOccurred())

		// Verify volume label (ISO9660 pads with null bytes).
		label := strings.TrimRight(fs.Label(), "\x00 ")
		Expect(label).To(Equal("cidata"))

		// ISO9660 with Rock Ridge preserves lowercase names.
		// Try lowercase first, fall back to uppercase (standard ISO9660).
		metaDataPath := "/meta-data"
		if _, err := fs.OpenFile(metaDataPath, os.O_RDONLY); err != nil {
			metaDataPath = "/META-DATA"
		}

		// Verify meta-data.
		metaData := readISOFile(fs, metaDataPath)
		Expect(metaData).To(HavePrefix("instance-id: kvarn-"))

		// Verify user-data.
		userDataPath := "/user-data"
		if _, err := fs.OpenFile(userDataPath, os.O_RDONLY); err != nil {
			userDataPath = "/USER_DATA.;1"
		}
		userData := readISOFile(fs, userDataPath)
		Expect(userData).To(ContainSubstring("#cloud-config"))
		Expect(userData).To(ContainSubstring("--token " + token))
		Expect(userData).To(ContainSubstring("--vsock-port 1024"))
		Expect(userData).NotTo(ContainSubstring("kvarn-proxy.crt"))
	})

	It("includes the proxy CA when provided", func() {
		path := GinkgoT().TempDir() + "/cidata.iso"

		const ca = "-----BEGIN CERTIFICATE-----\nFAKEDATA\n-----END CERTIFICATE-----\n"
		Expect(disk.CreateCloudInitDisk(path, disk.CloudInitOpts{
			Token:     "tok",
			VsockPort: 1024,
			ProxyCA:   []byte(ca),
		})).To(Succeed())

		d, err := diskfs.Open(path)
		Expect(err).NotTo(HaveOccurred())
		defer d.Close()

		fs, err := d.GetFilesystem(0)
		Expect(err).NotTo(HaveOccurred())

		userDataPath := "/user-data"
		if _, err := fs.OpenFile(userDataPath, os.O_RDONLY); err != nil {
			userDataPath = "/USER_DATA.;1"
		}
		userData := readISOFile(fs, userDataPath)
		Expect(userData).To(ContainSubstring("/usr/local/share/ca-certificates/kvarn-proxy.crt"))
		Expect(userData).To(ContainSubstring("update-ca-certificates"))
		Expect(userData).To(ContainSubstring("FAKEDATA"))
	})
})

func readISOFile(fs filesystem.FileSystem, name string) string {
	f, err := fs.OpenFile(name, os.O_RDONLY)
	Expect(err).NotTo(HaveOccurred(), "open %s", name)
	data, err := io.ReadAll(f)
	Expect(err).NotTo(HaveOccurred(), "read %s", name)
	return string(data)
}
