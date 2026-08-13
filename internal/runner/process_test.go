package runner_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/runner"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// processRecorder collects the callbacks a started process fires, so specs can
// assert on output and exit without racing the pumps.
type processRecorder struct {
	mu       sync.Mutex
	stdout   strings.Builder
	stderr   strings.Builder
	exited   bool
	exitCode int32
	exitErr  error
}

func (r *processRecorder) onOutput(_ string, stdout, stderr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stdout.WriteString(stdout)
	r.stderr.WriteString(stderr)
}

func (r *processRecorder) onExit(_ string, code int32, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exited = true
	r.exitCode = code
	r.exitErr = err
}

func (r *processRecorder) Stdout() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stdout.String()
}

func (r *processRecorder) Stderr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stderr.String()
}

func (r *processRecorder) Exit() (bool, int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exited, r.exitCode
}

// pidAlive reports whether a process is still running, without reaping it.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

var _ = Describe("Long-lived processes", func() {
	var (
		h   *runner.Handler
		ctx context.Context
	)

	BeforeEach(func() {
		h = runner.NewUnprivilegedHandler()
		ctx = context.Background()
	})

	AfterEach(func() {
		h.Close()
	})

	start := func(id, command string, rec *processRecorder) *v1.StartProcessResponse {
		GinkgoHelper()
		resp, err := h.StartProcessWithCallbacks(ctx, &v1.StartProcessRequest{
			ProcessId: id,
			Name:      id,
			Command:   command,
		}, rec.onOutput, rec.onExit)
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}

	It("streams output from a process that keeps running", func() {
		rec := &processRecorder{}
		resp := start("p1", "i=0; while true; do echo line-$i; i=$((i+1)); sleep 0.05; done", rec)
		Expect(resp.Pid).To(BeNumerically(">", 0))

		Eventually(rec.Stdout).Should(ContainSubstring("line-0"))
		Eventually(rec.Stdout).Should(ContainSubstring("line-2"))

		// Still running: nothing has reported an exit.
		exited, _ := rec.Exit()
		Expect(exited).To(BeFalse())
	})

	It("separates stderr from stdout", func() {
		rec := &processRecorder{}
		start("p-streams", "echo out; echo err >&2; sleep 30", rec)

		Eventually(rec.Stdout).Should(ContainSubstring("out"))
		Eventually(rec.Stderr).Should(ContainSubstring("err"))
		Expect(rec.Stdout()).NotTo(ContainSubstring("err"))
	})

	It("reports the exit of a process that ends on its own", func() {
		rec := &processRecorder{}
		start("p-short", "echo done; exit 3", rec)

		Eventually(func() bool { exited, _ := rec.Exit(); return exited }).Should(BeTrue())
		_, code := rec.Exit()
		Expect(code).To(Equal(int32(3)))
		Expect(rec.Stdout()).To(ContainSubstring("done"))
	})

	It("stops the whole process group, not just the leader", func() {
		dir := GinkgoT().TempDir()
		childPidFile := filepath.Join(dir, "child.pid")

		rec := &processRecorder{}
		// The shell backgrounds a child and then waits; killing only the
		// leader would leave the child behind.
		resp := start("p-group", fmt.Sprintf(
			"sh -c 'echo $$ > %s; sleep 300' & echo started; wait", childPidFile), rec)

		Eventually(rec.Stdout).Should(ContainSubstring("started"))
		Eventually(func() bool {
			data, err := os.ReadFile(childPidFile)
			return err == nil && len(strings.TrimSpace(string(data))) > 0
		}).Should(BeTrue())

		data, err := os.ReadFile(childPidFile)
		Expect(err).NotTo(HaveOccurred())
		childPid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		Expect(err).NotTo(HaveOccurred())
		Expect(pidAlive(childPid)).To(BeTrue())

		stopResp, err := h.StopProcess(ctx, connect.NewRequest(&v1.StopProcessRequest{
			ProcessId:    "p-group",
			GraceSeconds: 2,
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(stopResp.Msg.ExitCode).NotTo(Equal(int32(0)))

		Expect(pidAlive(int(resp.Pid))).To(BeFalse())
		Eventually(func() bool { return pidAlive(childPid) }).Should(BeFalse())

		Eventually(func() bool { exited, _ := rec.Exit(); return exited }).Should(BeTrue())
	})

	It("lists running and exited processes", func() {
		running := &processRecorder{}
		start("p-running", "sleep 30", running)
		gone := &processRecorder{}
		start("p-gone", "exit 7", gone)

		Eventually(func() bool { exited, _ := gone.Exit(); return exited }).Should(BeTrue())

		resp, err := h.ListProcesses(ctx, connect.NewRequest(&v1.ListProcessesRequest{}))
		Expect(err).NotTo(HaveOccurred())

		byID := map[string]*v1.ProcessInfo{}
		for _, p := range resp.Msg.Processes {
			byID[p.ProcessId] = p
		}
		Expect(byID).To(HaveKey("p-running"))
		Expect(byID["p-running"].Running).To(BeTrue())
		Expect(byID).To(HaveKey("p-gone"))
		Expect(byID["p-gone"].Running).To(BeFalse())
		Expect(byID["p-gone"].ExitCode).To(Equal(int32(7)))
	})

	It("refuses a duplicate process ID", func() {
		rec := &processRecorder{}
		start("p-dup", "sleep 30", rec)

		_, err := h.StartProcessWithCallbacks(ctx, &v1.StartProcessRequest{
			ProcessId: "p-dup",
			Command:   "sleep 30",
		}, nil, nil)
		Expect(err).To(HaveOccurred())
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeAlreadyExists))
	})

	It("requires a process ID and a command", func() {
		_, err := h.StartProcessWithCallbacks(ctx, &v1.StartProcessRequest{Command: "true"}, nil, nil)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))

		_, err = h.StartProcessWithCallbacks(ctx, &v1.StartProcessRequest{ProcessId: "x", Command: "  "}, nil, nil)
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
	})

	It("runs the process in the requested directory with the requested env", func() {
		dir := GinkgoT().TempDir()
		rec := &processRecorder{}
		_, err := h.StartProcessWithCallbacks(ctx, &v1.StartProcessRequest{
			ProcessId:  "p-env",
			Command:    "pwd; echo $KVARN_TEST_VALUE; sleep 30",
			WorkingDir: dir,
			Env:        map[string]string{"KVARN_TEST_VALUE": "hello world"},
		}, rec.onOutput, rec.onExit)
		Expect(err).NotTo(HaveOccurred())

		Eventually(rec.Stdout).Should(ContainSubstring("hello world"))
		// macOS resolves TempDir under /private; compare on the trailing path.
		Expect(rec.Stdout()).To(ContainSubstring(filepath.Base(dir)))
	})

	It("reports not-found when stopping an unknown process", func() {
		_, err := h.StopProcess(ctx, connect.NewRequest(&v1.StopProcessRequest{ProcessId: "nope"}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
	})

	It("reaps surviving processes when the handler closes", func() {
		rec := &processRecorder{}
		resp := start("p-survivor", "sleep 300", rec)
		Expect(pidAlive(int(resp.Pid))).To(BeTrue())

		h.Close()

		Expect(pidAlive(int(resp.Pid))).To(BeFalse())
		exited, _ := rec.Exit()
		Expect(exited).To(BeTrue())
	})

	It("returns immediately rather than waiting for the process", func() {
		rec := &processRecorder{}
		startedAt := time.Now()
		start("p-fast", "sleep 30", rec)
		Expect(time.Since(startedAt)).To(BeNumerically("<", 5*time.Second))
	})
})
