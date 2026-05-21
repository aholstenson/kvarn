package disk

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// CloudInitOpts configures the NoCloud seed disk handed to the VM.
type CloudInitOpts struct {
	Token     string
	VsockPort uint32

	// ProxyCA, if non-empty, is written into the rootfs as
	// /usr/local/share/ca-certificates/kvarn-proxy.crt and
	// update-ca-certificates is run on first boot so that in-VM TLS
	// clients trust the per-VM egress proxy.
	ProxyCA []byte
}

// CreateCloudInitDisk creates an ISO9660 image with the NoCloud datasource
// for cloud-init. The image contains meta-data and user-data files that
// configure the kvarn runner with the given token and vsock port and,
// optionally, install the per-VM proxy CA into the trust store.
func CreateCloudInitDisk(path string, opts CloudInitOpts) error {
	// 10 MB is more than enough for cloud-init seed data.
	const diskSize = 10 * 1024 * 1024

	d, err := diskfs.Create(path, diskSize, diskfs.SectorSizeDefault)
	if err != nil {
		return errors.Wrap(err, "create seed disk")
	}

	// ISO9660 requires a logical block size of 2048.
	d.LogicalBlocksize = 2048

	fspec := disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: "cidata",
	}
	fs, err := d.CreateFilesystem(fspec)
	if err != nil {
		return errors.Wrap(err, "create ISO9660 filesystem")
	}

	// Write meta-data.
	metaData := fmt.Sprintf("instance-id: kvarn-%d\n", time.Now().UnixNano())
	if err := writeISOFile(fs, "/meta-data", metaData); err != nil {
		return errors.Wrap(err, "write meta-data")
	}

	userData := buildUserData(opts)
	if err := writeISOFile(fs, "/user-data", userData); err != nil {
		return errors.Wrap(err, "write user-data")
	}

	// Static network config: the host-side userspace netstack acts as
	// the gateway and DNS forwarder at 10.0.2.1; the VM gets 10.0.2.2.
	if err := writeISOFile(fs, "/network-config", buildNetworkConfig()); err != nil {
		return errors.Wrap(err, "write network-config")
	}

	// Finalize the ISO with the cidata volume label.
	isoFS, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return errors.New("unexpected filesystem type")
	}

	if err := isoFS.Finalize(iso9660.FinalizeOptions{
		VolumeIdentifier: "cidata",
		RockRidge:        true,
	}); err != nil {
		return errors.Wrap(err, "finalize ISO")
	}

	return nil
}

func buildUserData(opts CloudInitOpts) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /run/kvarn-runner.env\n")
	b.WriteString("    content: |\n")
	fmt.Fprintf(&b, "      KVARN_RUNNER_ARGS=--token %s --vsock-port %d\n", opts.Token, opts.VsockPort)
	if len(opts.ProxyCA) > 0 {
		b.WriteString("  - path: /usr/local/share/ca-certificates/kvarn-proxy.crt\n")
		b.WriteString("    permissions: '0644'\n")
		b.WriteString("    content: |\n")
		for _, line := range strings.Split(strings.TrimRight(string(opts.ProxyCA), "\n"), "\n") {
			b.WriteString("      ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString("runcmd:\n")
		b.WriteString("  - update-ca-certificates\n")
	}
	return b.String()
}

func buildNetworkConfig() string {
	return `version: 2
ethernets:
  eth0:
    match:
      name: "en*"
    dhcp4: no
    addresses: [10.0.2.2/24]
    routes:
      - to: default
        via: 10.0.2.1
    nameservers:
      addresses: [10.0.2.1]
`
}

func writeISOFile(fs filesystem.FileSystem, name string, content string) error {
	f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	_, err = f.Write([]byte(content))
	return err
}
