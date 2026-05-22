package disk

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// CloudInitOpts configures the NoCloud seed disk handed to the VM.
type CloudInitOpts struct {
	Token     string
	VsockPort uint32

	// Runner, if non-empty, is the kvarn runner binary written to the seed as
	// a raw /kvarn-runner file. The in-VM setup script stages it to
	// /usr/local/bin/kvarn at boot, so the orchestrator ships the exact runner
	// it speaks to and the image carries none. cloud-init only reads
	// meta-data/user-data/network-config, so this extra file rides along
	// untouched.
	Runner []byte

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
	// diskfs.Create pre-sizes the backing file and leaves it at that size, so
	// the seed must be large enough to hold everything written to it. 10 MiB is
	// ample for the cloud-init text; when a runner binary is injected, add it
	// plus headroom for ISO9660 metadata. The Virtualization framework rejects
	// raw disk images whose size is not a multiple of the sector size, so round
	// up to the 2048-byte ISO9660 logical block (also a multiple of 512).
	const minDiskSize = 10 * 1024 * 1024
	const runnerHeadroom = 8 * 1024 * 1024
	const isoBlockSize = 2048
	diskSize := int64(minDiskSize)
	if want := int64(len(opts.Runner)) + runnerHeadroom; want > diskSize {
		diskSize = want
	}
	diskSize = (diskSize + isoBlockSize - 1) / isoBlockSize * isoBlockSize

	d, err := diskfs.Create(path, diskSize, diskfs.SectorSizeDefault)
	if err != nil {
		return fmt.Errorf("create seed disk: %w", err)
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
		return fmt.Errorf("create ISO9660 filesystem: %w", err)
	}

	// Write meta-data.
	metaData := fmt.Sprintf("instance-id: kvarn-%d\n", time.Now().UnixNano())
	if err := writeISOFile(fs, "/meta-data", metaData); err != nil {
		return fmt.Errorf("write meta-data: %w", err)
	}

	userData := buildUserData(opts)
	if err := writeISOFile(fs, "/user-data", userData); err != nil {
		return fmt.Errorf("write user-data: %w", err)
	}

	// Static network config: the host-side userspace netstack acts as
	// the gateway and DNS forwarder at 10.0.2.1; the VM gets 10.0.2.2.
	if err := writeISOFile(fs, "/network-config", buildNetworkConfig()); err != nil {
		return fmt.Errorf("write network-config: %w", err)
	}

	// Inject the runner binary as a raw file the in-VM setup script stages.
	if len(opts.Runner) > 0 {
		if err := writeISOBinaryFile(fs, "/kvarn-runner", opts.Runner); err != nil {
			return fmt.Errorf("write runner: %w", err)
		}
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
		return fmt.Errorf("finalize ISO: %w", err)
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
	return writeISOBinaryFile(fs, name, []byte(content))
}

func writeISOBinaryFile(fs filesystem.FileSystem, name string, content []byte) error {
	f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	return err
}
