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
		// The token must live in an env var, not on argv — anything on argv ends
		// up in /proc/<pid>/cmdline, which is world-readable on stock Linux and
		// would let any in-VM process impersonate the runner.
		Expect(userData).NotTo(ContainSubstring("--token"))
		Expect(userData).NotTo(ContainSubstring("--vsock-port"))
		Expect(userData).To(ContainSubstring("KVARN_BRIDGE_TOKEN=" + token))
		Expect(userData).To(ContainSubstring("KVARN_BRIDGE_VSOCK_PORT=1024"))
		// The env file must be locked down so the kvarn user (which runs job
		// steps) can't read the bearer token out of /run.
		Expect(userData).To(ContainSubstring("permissions: '0600'"))
		Expect(userData).To(ContainSubstring("owner: 'root:root'"))
		Expect(userData).NotTo(ContainSubstring("kvarn-proxy.crt"))
	})

	It("writes a registries.conf mirror block when ImageCacheAddr is set", func() {
		path := GinkgoT().TempDir() + "/cidata.iso"

		Expect(disk.CreateCloudInitDisk(path, disk.CloudInitOpts{
			Token:               "tok",
			VsockPort:           1024,
			ImageCacheAddr:      "10.0.2.1:5000",
			ImageCacheUpstreams: []string{"docker.io", "ghcr.io"},
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
		Expect(userData).To(ContainSubstring("/etc/containers/registries.conf.d/01-mirrors.conf"))
		Expect(userData).To(ContainSubstring(`location = "docker.io"`))
		Expect(userData).To(ContainSubstring(`location = "ghcr.io"`))
		// Mirror location embeds the upstream suffix so the cache's
		// /v2/<upstream>/<repo>/... routing receives the upstream name.
		Expect(userData).To(ContainSubstring(`location = "10.0.2.1:5000/docker.io"`))
		Expect(userData).To(ContainSubstring(`location = "10.0.2.1:5000/ghcr.io"`))
		Expect(userData).To(ContainSubstring("insecure = true"))
	})

	// The egress proxy CA is installed over the runner connection instead
	// (sandbox.InstallProxyCA), which is the only way to order trust ahead
	// of the first guest command that speaks TLS. A boot-time install here
	// would both land too late and collide with that one: two concurrent
	// update-ca-certificates runs fight over a fixed temp-file name.
	It("leaves the guest trust store alone", func() {
		path := GinkgoT().TempDir() + "/cidata.iso"

		Expect(disk.CreateCloudInitDisk(path, disk.CloudInitOpts{
			Token:     "tok",
			VsockPort: 1024,
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
		Expect(userData).NotTo(ContainSubstring("ca-certificates"))
	})
})

func readISOFile(fs filesystem.FileSystem, name string) string {
	f, err := fs.OpenFile(name, os.O_RDONLY)
	Expect(err).NotTo(HaveOccurred(), "open %s", name)
	data, err := io.ReadAll(f)
	Expect(err).NotTo(HaveOccurred(), "read %s", name)
	return string(data)
}
