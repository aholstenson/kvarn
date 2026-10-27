package sandbox_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aholstenson/kvarn/internal/sandbox"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The process scripts act on every process of the current user, which on a
// developer machine is the whole login. These tests put a fake ps first on
// PATH that lists only processes the test started itself, so the real kill
// can only reach those.
var _ = Describe("Leftover processes", func() {
	var (
		runner  *hostExecRunner
		pidFile string
		started []*exec.Cmd
	)

	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "60")
		Expect(cmd.Start()).To(Succeed())
		started = append(started, cmd)
		return cmd
	}
	pid := func(cmd *exec.Cmd) string { return strconv.Itoa(cmd.Process.Pid) }
	list := func(cmds ...*exec.Cmd) {
		pids := make([]string, 0, len(cmds))
		for _, c := range cmds {
			pids = append(pids, pid(c))
		}
		Expect(os.WriteFile(pidFile, []byte(strings.Join(pids, "\n")+"\n"), 0o644)).To(Succeed())
	}

	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		pidFile = filepath.Join(dir, "pids")
		Expect(os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\ncat \"$PIDFILE\"\n"), 0o755)).To(Succeed())
		runner = &hostExecRunner{
			mockRunnerProxy: newMockRunner(),
			env:             []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "PIDFILE=" + pidFile},
		}
		started = nil
	})

	AfterEach(func() {
		for _, cmd := range started {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	It("lists the processes ps reports", func() {
		before := start()
		list(before)
		pids, err := sandbox.UserProcesses(context.Background(), runner)
		Expect(err).NotTo(HaveOccurred())
		Expect(pids).To(Equal([]string{pid(before)}))
	})

	It("kills the processes that started after the listing and keeps the rest", func() {
		before := start()
		after := start()
		list(before, after)

		killed, err := sandbox.KillProcessesExcept(context.Background(), runner, []string{pid(before)})
		Expect(err).NotTo(HaveOccurred())
		Expect(killed).To(Equal(1))

		Expect(after.Wait()).To(MatchError(ContainSubstring("killed")))
		Expect(before.ProcessState).To(BeNil(), "the kept process is still running")
	})
})
