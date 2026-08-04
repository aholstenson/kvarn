package local_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/vm/local"
)

var _ = Describe("sweepStaleVMFiles", func() {
	var dir string

	write := func(name string) string {
		path := filepath.Join(dir, name)
		Expect(os.WriteFile(path, []byte("x"), 0o600)).To(Succeed())
		return path
	}

	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("removes every file a local VM leaves behind", func() {
		// The disk file carries the prefix and the rest are derived from its
		// name, which is what lets two patterns cover all of them.
		leftovers := []string{
			write("kvarn-disk-123.qcow2"),
			write("kvarn-disk-123.qcow2.cidata.iso"),
			write("kvarn-disk-123.qcow2.qmp"),
			write("kvarn-disk-456.img"),
			write("kvarn-disk-456.img.nvram"),
			write("kvarn-ovmf-vars-789.fd"),
		}

		local.SweepStaleVMFiles(dir)

		for _, path := range leftovers {
			Expect(exists(path)).To(BeFalse(), "expected %s to be swept", path)
		}
	})

	It("leaves files it does not own alone", func() {
		keep := []string{
			write("disk.qcow2"),
			write("kvarn-sessions.db"),
			write("some-other-tool.img"),
		}

		local.SweepStaleVMFiles(dir)

		for _, path := range keep {
			Expect(exists(path)).To(BeTrue(), "expected %s to survive", path)
		}
	})

	It("is a no-op on a directory with nothing to sweep", func() {
		Expect(func() { local.SweepStaleVMFiles(dir) }).NotTo(Panic())
	})
})
