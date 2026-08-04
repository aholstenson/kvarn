package localsock_test

import (
	"net"
	"os"
	"path/filepath"

	"github.com/aholstenson/kvarn/internal/localsock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Listen", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	It("creates a socket that can be dialled", func() {
		path := filepath.Join(dir, "sock")
		l, err := localsock.Listen(path)
		Expect(err).NotTo(HaveOccurred())
		defer l.Close()

		Expect(localsock.Exists(path)).To(BeTrue())

		conn, err := net.Dial("unix", path)
		Expect(err).NotTo(HaveOccurred())
		conn.Close()
	})

	// The permissions are the whole authentication story: they are what makes
	// arriving on this socket a claim worth believing.
	It("restricts the socket and its directory to the owner", func() {
		path := filepath.Join(dir, "nested", "sock")
		l, err := localsock.Listen(path)
		Expect(err).NotTo(HaveOccurred())
		defer l.Close()

		fi, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o600)))

		di, err := os.Stat(filepath.Dir(path))
		Expect(err).NotTo(HaveOccurred())
		Expect(di.Mode().Perm()).To(Equal(os.FileMode(0o700)))
	})

	It("replaces a socket file left behind by a dead process", func() {
		path := filepath.Join(dir, "sock")
		first, err := localsock.Listen(path)
		Expect(err).NotTo(HaveOccurred())
		// Closing the listener normally unlinks the file; put it back to stand
		// in for a process that died without getting the chance.
		Expect(first.Close()).To(Succeed())
		Expect(os.WriteFile(path, nil, 0o600)).To(Succeed())

		second, err := localsock.Listen(path)
		Expect(err).NotTo(HaveOccurred())
		defer second.Close()

		conn, err := net.Dial("unix", path)
		Expect(err).NotTo(HaveOccurred())
		conn.Close()
	})

	// Removing a socket a live orchestrator is serving would leave it
	// listening on a path nothing can reach, which is worse than refusing.
	It("refuses to take over a socket another process is serving", func() {
		path := filepath.Join(dir, "sock")
		first, err := localsock.Listen(path)
		Expect(err).NotTo(HaveOccurred())
		defer first.Close()

		_, err = localsock.Listen(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("already serving"))

		// The live listener is untouched.
		conn, err := net.Dial("unix", path)
		Expect(err).NotTo(HaveOccurred())
		conn.Close()
	})

	It("unlinks the socket on close so a restart finds nothing stale", func() {
		path := filepath.Join(dir, "sock")
		l, err := localsock.Listen(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.Close()).To(Succeed())
		Expect(localsock.Exists(path)).To(BeFalse())
	})
})

var _ = Describe("Address parsing", func() {
	It("round-trips a path through Address/Path", func() {
		path, ok := localsock.Path(localsock.Address("/run/kvarn.sock"))
		Expect(ok).To(BeTrue())
		Expect(path).To(Equal("/run/kvarn.sock"))
	})

	// Only the explicit scheme selects the socket transport; otherwise the
	// shape of a string would decide how a command talks to the orchestrator.
	It("does not treat a URL or a bare path as a socket", func() {
		_, ok := localsock.Path("http://localhost:8080")
		Expect(ok).To(BeFalse())
		_, ok = localsock.Path("/run/kvarn.sock")
		Expect(ok).To(BeFalse())
	})

	It("reports a non-socket file as not a socket", func() {
		path := filepath.Join(GinkgoT().TempDir(), "regular")
		Expect(os.WriteFile(path, nil, 0o600)).To(Succeed())
		Expect(localsock.Exists(path)).To(BeFalse())
	})
})
