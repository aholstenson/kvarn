//go:build linux

package disk_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/disk"
)

const mib = 1024 * 1024

// makeBaseImage creates a qcow2 image to overlay, skipping the spec when
// qemu-img is unavailable.
func makeBaseImage(path string, sizeBytes int64) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		Skip("qemu-img not installed")
	}
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", path, fmt.Sprintf("%d", sizeBytes))
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
}

var _ = Describe("CreateOverlayQcow2", func() {
	var dir, base string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		base = filepath.Join(dir, "base.qcow2")
		makeBaseImage(base, 64*mib)
	})

	It("creates an overlay far smaller than the image it reads through to", func() {
		overlay := filepath.Join(dir, "overlay.qcow2")
		Expect(disk.CreateOverlayQcow2(overlay, base, 256*mib)).To(Succeed())

		info, err := os.Stat(overlay)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Size()).To(BeNumerically("<", mib))
	})

	It("exposes the requested virtual size to the guest", func() {
		overlay := filepath.Join(dir, "overlay.qcow2")
		Expect(disk.CreateOverlayQcow2(overlay, base, 256*mib)).To(Succeed())

		Expect(disk.Qcow2VirtualSize(overlay)).To(Equal(int64(256 * mib)))
	})

	It("raises a requested size below the backing size", func() {
		overlay := filepath.Join(dir, "overlay.qcow2")
		Expect(disk.CreateOverlayQcow2(overlay, base, 16*mib)).To(Succeed())

		Expect(disk.Qcow2VirtualSize(overlay)).To(Equal(int64(64 * mib)))
	})

	It("overwrites the placeholder file left by os.CreateTemp", func() {
		overlay := filepath.Join(dir, "overlay.qcow2")
		Expect(os.WriteFile(overlay, nil, 0o600)).To(Succeed())

		Expect(disk.CreateOverlayQcow2(overlay, base, 256*mib)).To(Succeed())
		Expect(disk.Qcow2VirtualSize(overlay)).To(Equal(int64(256 * mib)))
	})

	It("records the backing file so the overlay resolves from any directory", func() {
		// The overlay lives in a temp directory while the base image lives in
		// the image cache, so a backing path stored as given would not resolve.
		relBase, err := filepath.Rel(mustGetwd(), base)
		Expect(err).NotTo(HaveOccurred())

		overlay := filepath.Join(dir, "overlay.qcow2")
		Expect(disk.CreateOverlayQcow2(overlay, relBase, 256*mib)).To(Succeed())

		cmd := exec.Command("qemu-img", "check", overlay)
		cmd.Dir = "/"
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
	})

	It("fails when the backing image does not exist", func() {
		overlay := filepath.Join(dir, "overlay.qcow2")
		err := disk.CreateOverlayQcow2(overlay, filepath.Join(dir, "missing.qcow2"), 256*mib)
		Expect(err).To(HaveOccurred())
	})
})

func mustGetwd() string {
	wd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	return wd
}
