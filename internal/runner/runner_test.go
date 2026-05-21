package runner_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/gen/kvarn/v1/kvarnv1connect"
	"github.com/aholstenson/kvarn/internal/runner"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Runner", func() {
	var (
		client kvarnv1connect.RunnerServiceClient
		server *http.Server
		addr   string
	)

	BeforeEach(func() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		addr = listener.Addr().String()

		server = runner.NewServer()
		go server.Serve(listener)

		client = kvarnv1connect.NewRunnerServiceClient(http.DefaultClient, fmt.Sprintf("http://%s", addr))
	})

	AfterEach(func() {
		server.Close()
	})

	It("executes a command successfully", func() {
		resp, err := client.Exec(context.Background(), connect.NewRequest(&v1.ExecRequest{
			Command: "echo",
			Args:    []string{"hello", "world"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).To(Equal(int32(0)))
		Expect(resp.Msg.Stdout).To(Equal("hello world\n"))
		Expect(resp.Msg.Stderr).To(BeEmpty())
	})

	It("captures stderr", func() {
		resp, err := client.Exec(context.Background(), connect.NewRequest(&v1.ExecRequest{
			Command: "sh",
			Args:    []string{"-c", "echo error >&2"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).To(Equal(int32(0)))
		Expect(resp.Msg.Stderr).To(Equal("error\n"))
	})

	It("returns non-zero exit code", func() {
		resp, err := client.Exec(context.Background(), connect.NewRequest(&v1.ExecRequest{
			Command: "sh",
			Args:    []string{"-c", "exit 42"},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).To(Equal(int32(42)))
	})

	It("returns non-zero exit code for invalid command", func() {
		resp, err := client.Exec(context.Background(), connect.NewRequest(&v1.ExecRequest{
			Command: "nonexistent-command-xyz",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).NotTo(Equal(int32(0)))
	})

	It("respects working directory", func() {
		resp, err := client.Exec(context.Background(), connect.NewRequest(&v1.ExecRequest{
			Command:    "pwd",
			WorkingDir: "/tmp",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.ExitCode).To(Equal(int32(0)))
		// macOS /tmp is a symlink to /private/tmp
		Expect(resp.Msg.Stdout).To(SatisfyAny(Equal("/tmp\n"), Equal("/private/tmp\n")))
	})

	It("handles context cancellation", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := client.Exec(ctx, connect.NewRequest(&v1.ExecRequest{
			Command: "sleep",
			Args:    []string{"10"},
		}))
		Expect(err).To(HaveOccurred())
	})

	Describe("UploadFiles", func() {
		var workDir string

		BeforeEach(func() {
			var err error
			workDir, err = os.MkdirTemp("", "runner-test-*")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(workDir)
		})

		It("uploads multiple files", func() {
			resp, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "a.txt", Content: []byte("hello")},
					{Path: "b.txt", Content: []byte("world")},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.FilesWritten).To(Equal(int32(2)))

			content, err := os.ReadFile(filepath.Join(workDir, "a.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("hello"))

			content, err = os.ReadFile(filepath.Join(workDir, "b.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("world"))
		})

		It("creates nested directories automatically", func() {
			resp, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "sub/dir/file.txt", Content: []byte("nested")},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.FilesWritten).To(Equal(int32(1)))

			content, err := os.ReadFile(filepath.Join(workDir, "sub", "dir", "file.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("nested"))
		})

		It("rejects path traversal", func() {
			_, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "../../../etc/passwd", Content: []byte("evil")},
				},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("uploads binary content", func() {
			binData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
			resp, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "binary.bin", Content: binData},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.FilesWritten).To(Equal(int32(1)))

			content, err := os.ReadFile(filepath.Join(workDir, "binary.bin"))
			Expect(err).NotTo(HaveOccurred())
			Expect(content).To(Equal(binData))
		})
	})

	Describe("ReadFile", func() {
		var workDir string

		BeforeEach(func() {
			var err error
			workDir, err = os.MkdirTemp("", "runner-test-*")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(workDir)
		})

		It("returns tagged lines and a version hash", func() {
			_, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "test.txt", Content: []byte("hello\nworld\n")},
				},
			}))
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.ReadFile(context.Background(), connect.NewRequest(&v1.ReadFileRequest{
				WorkingDir: workDir,
				Path:       "test.txt",
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Version).NotTo(BeEmpty())
			Expect(resp.Msg.TotalLines).To(Equal(int32(2)))
			Expect(resp.Msg.Lines).To(HaveLen(2))
			Expect(resp.Msg.Lines[0].Content).To(Equal("hello"))
			Expect(resp.Msg.Lines[0].Hash).NotTo(BeEmpty())
		})

		It("returns CodeNotFound for missing file", func() {
			_, err := client.ReadFile(context.Background(), connect.NewRequest(&v1.ReadFileRequest{
				WorkingDir: workDir,
				Path:       "nonexistent.txt",
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		})

		It("rejects non-UTF-8 content", func() {
			binData := []byte{0xFF, 0xFE, 0xFD}
			_, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "binary.bin", Content: binData},
				},
			}))
			Expect(err).NotTo(HaveOccurred())

			_, err = client.ReadFile(context.Background(), connect.NewRequest(&v1.ReadFileRequest{
				WorkingDir: workDir,
				Path:       "binary.bin",
			}))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("EditFile", func() {
		var workDir string

		BeforeEach(func() {
			var err error
			workDir, err = os.MkdirTemp("", "runner-test-*")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(workDir)
		})

		It("applies a single REPLACE op", func() {
			_, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "test.txt", Content: []byte("hello\nworld\n")},
				},
			}))
			Expect(err).NotTo(HaveOccurred())

			read, err := client.ReadFile(context.Background(), connect.NewRequest(&v1.ReadFileRequest{
				WorkingDir: workDir,
				Path:       "test.txt",
			}))
			Expect(err).NotTo(HaveOccurred())

			resp, err := client.EditFile(context.Background(), connect.NewRequest(&v1.EditFileRequest{
				WorkingDir:      workDir,
				Path:            "test.txt",
				ExpectedVersion: read.Msg.Version,
				Operations: []*v1.EditOperation{
					{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: read.Msg.Lines[0].Hash, Lines: []string{"hi"}},
				},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Version).NotTo(BeEmpty())
			Expect(resp.Msg.TotalLines).To(Equal(int32(2)))

			content, err := os.ReadFile(filepath.Join(workDir, "test.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("hi\nworld\n"))
		})

		It("rejects edits with stale version", func() {
			_, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "test.txt", Content: []byte("hello\n")},
				},
			}))
			Expect(err).NotTo(HaveOccurred())

			read, err := client.ReadFile(context.Background(), connect.NewRequest(&v1.ReadFileRequest{
				WorkingDir: workDir,
				Path:       "test.txt",
			}))
			Expect(err).NotTo(HaveOccurred())

			_, err = client.EditFile(context.Background(), connect.NewRequest(&v1.EditFileRequest{
				WorkingDir:      workDir,
				Path:            "test.txt",
				ExpectedVersion: "deadbeef",
				Operations: []*v1.EditOperation{
					{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: read.Msg.Lines[0].Hash, Lines: []string{"hi"}},
				},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
		})

		It("rejects path traversal", func() {
			_, err := client.EditFile(context.Background(), connect.NewRequest(&v1.EditFileRequest{
				WorkingDir: workDir,
				Path:       "../../../etc/passwd",
				Operations: []*v1.EditOperation{
					{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: "ab", Lines: []string{"evil"}},
				},
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("errors when operations is empty", func() {
			_, err := client.UploadFiles(context.Background(), connect.NewRequest(&v1.UploadFilesRequest{
				WorkingDir: workDir,
				Files: []*v1.FileContent{
					{Path: "test.txt", Content: []byte("hello\n")},
				},
			}))
			Expect(err).NotTo(HaveOccurred())

			_, err = client.EditFile(context.Background(), connect.NewRequest(&v1.EditFileRequest{
				WorkingDir: workDir,
				Path:       "test.txt",
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})
	})

	Describe("WriteFile", func() {
		var workDir string

		BeforeEach(func() {
			var err error
			workDir, err = os.MkdirTemp("", "runner-test-*")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			os.RemoveAll(workDir)
		})

		It("creates a new file when expected_version is empty", func() {
			resp, err := client.WriteFile(context.Background(), connect.NewRequest(&v1.WriteFileRequest{
				WorkingDir: workDir,
				Path:       "new.txt",
				Content:    []byte("hello\n"),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.Version).NotTo(BeEmpty())
			Expect(resp.Msg.TotalLines).To(Equal(int32(1)))

			content, err := os.ReadFile(filepath.Join(workDir, "new.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("hello\n"))
		})

		It("refuses to overwrite an existing file without a version", func() {
			_, err := client.WriteFile(context.Background(), connect.NewRequest(&v1.WriteFileRequest{
				WorkingDir: workDir,
				Path:       "a.txt",
				Content:    []byte("first\n"),
			}))
			Expect(err).NotTo(HaveOccurred())

			_, err = client.WriteFile(context.Background(), connect.NewRequest(&v1.WriteFileRequest{
				WorkingDir: workDir,
				Path:       "a.txt",
				Content:    []byte("second\n"),
			}))
			Expect(err).To(HaveOccurred())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
		})

		It("overwrites with matching expected_version", func() {
			resp, err := client.WriteFile(context.Background(), connect.NewRequest(&v1.WriteFileRequest{
				WorkingDir: workDir,
				Path:       "a.txt",
				Content:    []byte("first\n"),
			}))
			Expect(err).NotTo(HaveOccurred())

			_, err = client.WriteFile(context.Background(), connect.NewRequest(&v1.WriteFileRequest{
				WorkingDir:      workDir,
				Path:            "a.txt",
				Content:         []byte("second\n"),
				ExpectedVersion: resp.Msg.Version,
			}))
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(filepath.Join(workDir, "a.txt"))
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal("second\n"))
		})
	})
})
