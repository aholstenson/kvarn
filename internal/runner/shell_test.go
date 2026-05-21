package runner_test

import (
	"context"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/runner"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Shell Sessions", func() {
	var (
		h   *runner.Handler // exported type
		ctx context.Context
	)

	BeforeEach(func() {
		h = runner.NewHandler()
		ctx = context.Background()
	})

	AfterEach(func() {
		h.Close()
	})

	createSession := func() string {
		resp, err := h.CreateSession(ctx, connect.NewRequest(&v1.CreateSessionRequest{}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg.SessionId
	}

	sessionExec := func(id, command string) *v1.SessionExecResponse {
		resp, err := h.SessionExec(ctx, connect.NewRequest(&v1.SessionExecRequest{
			SessionId: id,
			Command:   command,
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}

	Describe("CreateSession", func() {
		It("creates a session and returns an ID", func() {
			id := createSession()
			Expect(id).NotTo(BeEmpty())
		})
	})

	Describe("SessionExec", func() {
		It("captures stdout", func() {
			id := createSession()
			resp := sessionExec(id, "echo hello")
			Expect(resp.Stdout).To(Equal("hello\n"))
			Expect(resp.ExitCode).To(Equal(int32(0)))
		})

		It("captures stderr", func() {
			id := createSession()
			resp := sessionExec(id, "echo err >&2")
			Expect(resp.Stderr).To(Equal("err\n"))
		})

		It("returns non-zero exit code", func() {
			id := createSession()
			resp := sessionExec(id, "false")
			Expect(resp.ExitCode).To(Equal(int32(1)))
		})

		It("handles exit command by respawning", func() {
			id := createSession()
			// `exit 42` kills the shell; the session should detect death and respawn.
			resp := sessionExec(id, "exit 42")
			Expect(resp.ExitCode).NotTo(Equal(int32(0)))

			// Verify the session is still usable after shell death.
			resp = sessionExec(id, "echo alive")
			Expect(resp.Stdout).To(Equal("alive\n"))
		})

		It("persists environment variables across calls", func() {
			id := createSession()
			sessionExec(id, "export FOO=bar")
			resp := sessionExec(id, "echo $FOO")
			Expect(resp.Stdout).To(Equal("bar\n"))
		})

		It("persists working directory across calls", func() {
			id := createSession()
			sessionExec(id, "cd /tmp")
			resp := sessionExec(id, "pwd")
			Expect(resp.Stdout).To(ContainSubstring("/tmp"))
			Expect(resp.WorkingDir).To(ContainSubstring("/tmp"))
		})

		It("handles pipes and redirects", func() {
			id := createSession()
			resp := sessionExec(id, "echo hello | tr a-z A-Z")
			Expect(resp.Stdout).To(Equal("HELLO\n"))
		})

		It("handles empty output", func() {
			id := createSession()
			resp := sessionExec(id, "true")
			Expect(resp.Stdout).To(Equal(""))
			Expect(resp.Stderr).To(Equal(""))
			Expect(resp.ExitCode).To(Equal(int32(0)))
		})

		It("handles multiline commands", func() {
			id := createSession()
			resp := sessionExec(id, "for i in 1 2 3; do\necho $i\ndone")
			Expect(resp.Stdout).To(Equal("1\n2\n3\n"))
		})

		It("runs multiple sequential commands", func() {
			id := createSession()
			for i := 0; i < 10; i++ {
				resp := sessionExec(id, "echo ok")
				Expect(resp.Stdout).To(Equal("ok\n"))
			}
		})

		It("returns error for timeout", func() {
			id := createSession()
			_, err := h.SessionExec(ctx, connect.NewRequest(&v1.SessionExecRequest{
				SessionId:      id,
				Command:        "sleep 60",
				TimeoutSeconds: 1,
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timed out"))
		})

		It("recovers after timeout", func() {
			id := createSession()
			_, err := h.SessionExec(ctx, connect.NewRequest(&v1.SessionExecRequest{
				SessionId:      id,
				Command:        "sleep 60",
				TimeoutSeconds: 1,
			}))
			Expect(err).To(HaveOccurred())

			// Should still work after timeout.
			resp := sessionExec(id, "echo recovered")
			Expect(resp.Stdout).To(Equal("recovered\n"))
		})

		It("returns error for unknown session", func() {
			_, err := h.SessionExec(ctx, connect.NewRequest(&v1.SessionExecRequest{
				SessionId: "nonexistent",
				Command:   "echo hello",
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("CloseSession", func() {
		It("closes session and cleans up", func() {
			id := createSession()
			_, err := h.CloseSession(ctx, connect.NewRequest(&v1.CloseSessionRequest{
				SessionId: id,
			}))
			Expect(err).NotTo(HaveOccurred())

			// Subsequent exec should fail.
			_, err = h.SessionExec(ctx, connect.NewRequest(&v1.SessionExecRequest{
				SessionId: id,
				Command:   "echo hello",
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("returns error for unknown session", func() {
			_, err := h.CloseSession(ctx, connect.NewRequest(&v1.CloseSessionRequest{
				SessionId: "nonexistent",
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})
})
