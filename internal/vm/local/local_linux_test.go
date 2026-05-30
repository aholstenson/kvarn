//go:build linux

package local_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/local"
	"github.com/aholstenson/kvarn/internal/vm/local/vmtable"
)

// makeProc writes a fake /proc/<pid>/cmdline entry under procRoot using NUL
// byte separators, mirroring the Linux kernel format.
func makeProc(procRoot, pid string, args ...string) {
	dir := filepath.Join(procRoot, pid)
	ExpectWithOffset(1, os.MkdirAll(dir, 0o755)).To(Succeed())
	cmdline := strings.Join(args, "\x00") + "\x00"
	ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644)).To(Succeed())
}

// makeProcWithComm writes a fake /proc/<pid> directory with both a cmdline
// and a comm file, since walkQEMUProcs now consults comm alongside cmdline.
func makeProcWithComm(procRoot, pid, comm string, args ...string) {
	makeProc(procRoot, pid, args...)
	dir := filepath.Join(procRoot, pid)
	ExpectWithOffset(1, os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644)).To(Succeed())
}

var _ = Describe("scanHighestQEMUCIDFromProc", func() {
	var procRoot string

	BeforeEach(func() {
		procRoot = GinkgoT().TempDir()
	})

	It("returns 0 when the proc directory is empty", func() {
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(BeZero())
	})

	It("returns 0 when the proc directory does not exist", func() {
		Expect(local.ScanHighestQEMUCIDFromProc(filepath.Join(procRoot, "nonexistent"))).To(BeZero())
	})

	It("ignores non-numeric proc entries such as 'self'", func() {
		dir := filepath.Join(procRoot, "self")
		Expect(os.MkdirAll(dir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "cmdline"),
			[]byte("qemu-system-x86_64\x00-device\x00vhost-vsock-pci,guest-cid=10\x00"), 0o644)).To(Succeed())
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(BeZero())
	})

	It("returns the CID from a single QEMU process", func() {
		makeProc(procRoot, "1234",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=5")
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(Equal(uint32(5)))
	})

	It("returns the highest CID across multiple QEMU processes", func() {
		makeProc(procRoot, "100",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=3")
		makeProc(procRoot, "101",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=7")
		makeProc(procRoot, "102",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=5")
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(Equal(uint32(7)))
	})

	It("returns 0 when no process has a vsock device", func() {
		makeProc(procRoot, "200", "bash", "-c", "sleep 100")
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(BeZero())
	})

	It("ignores processes without a vsock device and returns the highest among those that do", func() {
		makeProc(procRoot, "200", "bash", "-c", "sleep 100")
		makeProc(procRoot, "201",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=4")
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(Equal(uint32(4)))
	})

	It("tolerates a PID directory with no cmdline file", func() {
		Expect(os.MkdirAll(filepath.Join(procRoot, "999"), 0o755)).To(Succeed())
		Expect(local.ScanHighestQEMUCIDFromProc(procRoot)).To(BeZero())
	})
})

var _ = Describe("readCIDFromCmdline", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	writeCmdline := func(args ...string) string {
		path := filepath.Join(dir, "cmdline")
		content := strings.Join(args, "\x00") + "\x00"
		ExpectWithOffset(1, os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
		return path
	}

	It("returns 0 when the file does not exist", func() {
		Expect(local.ReadCIDFromCmdline(filepath.Join(dir, "missing"))).To(BeZero())
	})

	It("returns 0 for a process without a vsock device argument", func() {
		path := writeCmdline("bash", "-c", "sleep 100")
		Expect(local.ReadCIDFromCmdline(path)).To(BeZero())
	})

	It("extracts the guest-cid from a vhost-vsock-pci device argument", func() {
		path := writeCmdline("qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=8")
		Expect(local.ReadCIDFromCmdline(path)).To(Equal(uint32(8)))
	})

	It("returns 0 when the vhost-vsock-pci argument has no guest-cid field", func() {
		path := writeCmdline("qemu-system-x86_64", "-device", "vhost-vsock-pci,id=vsock0")
		Expect(local.ReadCIDFromCmdline(path)).To(BeZero())
	})

	It("ignores non-vhost-vsock-pci device arguments", func() {
		path := writeCmdline("qemu-system-x86_64",
			"-device", "virtio-net-pci,netdev=net0",
			"-device", "vhost-vsock-pci,guest-cid=12")
		Expect(local.ReadCIDFromCmdline(path)).To(Equal(uint32(12)))
	})
})

var _ = Describe("NewProvider CID seeding", func() {
	It("allocates CID 3 on first call when no VMs are running", func() {
		// highest=0 means no running VMs; seeding is skipped and nextCID stays 0.
		p := local.NewProviderWithHighestCID(0)
		Expect(p.AllocateCIDForTest()).To(Equal(uint32(3)))
	})

	It("allocates highest+1 on first call when VMs are already running", func() {
		// Simulate orphaned VMs holding CIDs 3-10; next allocation must be 11.
		p := local.NewProviderWithHighestCID(10)
		Expect(p.AllocateCIDForTest()).To(Equal(uint32(11)))
	})

	It("allocates sequentially after the initial seeded value", func() {
		p := local.NewProviderWithHighestCID(5)
		Expect(p.AllocateCIDForTest()).To(Equal(uint32(6)))
		Expect(p.AllocateCIDForTest()).To(Equal(uint32(7)))
		Expect(p.AllocateCIDForTest()).To(Equal(uint32(8)))
	})

	It("seeds correctly from a proc scan that finds running VMs", func() {
		procRoot := GinkgoT().TempDir()
		makeProc(procRoot, "42",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=10")

		highest := local.ScanHighestQEMUCIDFromProc(procRoot)
		Expect(highest).To(Equal(uint32(10)))

		p := local.NewProviderWithHighestCID(highest)
		Expect(p.AllocateCIDForTest()).To(Equal(uint32(11)))
	})
})

var _ = Describe("WalkQEMUProcs", func() {
	var procRoot string

	BeforeEach(func() {
		procRoot = GinkgoT().TempDir()
	})

	It("returns the pid, cid, and comm for matching processes", func() {
		makeProcWithComm(procRoot, "100", "qemu-system-x86",
			"qemu-system-x86_64", "-device", "vhost-vsock-pci,guest-cid=7")

		entries := local.WalkQEMUProcs(procRoot)
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].PID).To(Equal(100))
		Expect(entries[0].CID).To(Equal(uint32(7)))
		Expect(entries[0].Comm).To(Equal("qemu-system-x86"))
	})

	It("skips non-QEMU processes that don't expose a vsock device", func() {
		makeProcWithComm(procRoot, "200", "bash", "bash", "-c", "sleep 1")
		Expect(local.WalkQEMUProcs(procRoot)).To(BeEmpty())
	})

	It("keeps a QEMU process even when it has no guest-cid (comm match)", func() {
		makeProcWithComm(procRoot, "300", "qemu-system-x86",
			"qemu-system-x86_64", "-nographic")
		entries := local.WalkQEMUProcs(procRoot)
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].CID).To(BeZero())
	})
})

var _ = Describe("reapOrphans", func() {
	var tablePath string
	var tmpFiles []string

	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		tablePath = filepath.Join(dir, "vms.json")
		tmpFiles = nil
	})

	makeTmpFile := func() string {
		f, err := os.CreateTemp("", "kvarn-test-tmpfile-*")
		Expect(err).NotTo(HaveOccurred())
		f.Close()
		tmpFiles = append(tmpFiles, f.Name())
		return f.Name()
	}

	AfterEach(func() {
		for _, p := range tmpFiles {
			os.Remove(p)
		}
	})

	It("cleans up files and drops the entry when the recorded PID has been recycled", func() {
		// PID 1 is always alive but its comm will not match the recorded
		// value, so the reaper must treat the entry as recycled: clean up
		// our files without signalling pid 1.
		disk := makeTmpFile()
		seed := makeTmpFile()
		vars := makeTmpFile()
		qmp := makeTmpFile()

		table, err := vmtable.Open(tablePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(table.Add(vmtable.Entry{
			ID:      "recycled-vm",
			PID:     1,
			CID:     5,
			TmpDisk: disk,
			TmpSeed: seed,
			TmpVars: vars,
			QMPSock: qmp,
			Comm:    "qemu-system-x86",
		})).To(Succeed())

		local.ReapOrphans(table, nil)

		Expect(table.List()).To(BeEmpty())
		for _, p := range []string{disk, seed, vars, qmp} {
			_, err := os.Stat(p)
			Expect(os.IsNotExist(err)).To(BeTrue(), "expected %s to be removed", p)
		}
	})

	It("ignores untracked /proc entries that are not QEMU", func() {
		table, err := vmtable.Open(tablePath)
		Expect(err).NotTo(HaveOccurred())

		// A bash process with cid > 2 would never be produced by walkQEMUProcs
		// because we filter to qemu-system comms; pass it directly to confirm
		// reapOrphans applies the same comm filter for safety.
		Expect(func() {
			local.ReapOrphans(table, []local.ProcEntry{
				{PID: os.Getpid(), CID: 99, Comm: "go"},
			})
		}).NotTo(Panic())
	})

	It("cleans up after a real subprocess once it has exited", func() {
		// Spawn a child, kill it, then point reapOrphans at the leftover
		// table entry. The reaper must remove the file, drop the entry,
		// and not block on the dead pid.
		cmd := exec.Command("sh", "-c", "exec sleep 30")
		Expect(cmd.Start()).To(Succeed())
		pid := cmd.Process.Pid
		Expect(cmd.Process.Kill()).To(Succeed())
		_ = cmd.Wait()

		// Wait briefly for /proc to drop the entry.
		Eventually(func() bool {
			_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
			return os.IsNotExist(err)
		}, 2*time.Second, 50*time.Millisecond).Should(BeTrue())

		disk := makeTmpFile()
		table, err := vmtable.Open(tablePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(table.Add(vmtable.Entry{
			ID:      "vm-after-crash",
			PID:     pid,
			CID:     7,
			TmpDisk: disk,
			Comm:    "sleep",
		})).To(Succeed())

		local.ReapOrphans(table, nil)
		Expect(table.List()).To(BeEmpty())
		_, err = os.Stat(disk)
		Expect(os.IsNotExist(err)).To(BeTrue())
	})
})
