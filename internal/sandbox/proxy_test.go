package sandbox_test

import (
	"bytes"
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/dispatch"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

var _ = Describe("BridgeProxy", func() {
	var (
		proxy     *sandbox.BridgeProxy
		pr        *dispatch.PendingRunner
		commandCh chan *v1.RunnerCommand
		resultCh  chan *v1.CommandResult
		outputCh  chan *v1.OutputChunk
		ctx       context.Context
	)

	BeforeEach(func() {
		commandCh = make(chan *v1.RunnerCommand, 1)
		resultCh = make(chan *v1.CommandResult, 1)
		outputCh = make(chan *v1.OutputChunk, 64)
		pr = &dispatch.PendingRunner{
			CommandCh: commandCh,
			ResultCh:  resultCh,
			OutputCh:  outputCh,
			DoneCh:    make(chan struct{}),
		}
		proxy = sandbox.NewBridgeProxy(commandCh, resultCh, outputCh, pr)
		ctx = context.Background()
	})

	Describe("Exec", func() {
		It("sends command and receives result", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetExec()).NotTo(BeNil())
				Expect(cmd.GetExec().Command).To(Equal("echo"))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_Exec{
						Exec: &v1.ExecResponse{
							ExitCode: 0,
							Stdout:   "hello\n",
						},
					},
				}
			}()

			resp, err := proxy.Exec(ctx, &v1.ExecRequest{
				Command: "echo",
				Args:    []string{"hello"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Stdout).To(Equal("hello\n"))
			Expect(resp.ExitCode).To(Equal(int32(0)))
		})

		It("returns error from runner", func() {
			go func() {
				cmd := <-commandCh
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Error:     "command failed",
				}
			}()

			_, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "fail"})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("command failed"))
		})

		It("returns error on context cancellation", func() {
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel()

			_, err := proxy.Exec(cancelCtx, &v1.ExecRequest{Command: "echo"})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("UploadFiles", func() {
		It("sends upload and receives result", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetUploadFiles()).NotTo(BeNil())
				Expect(cmd.GetUploadFiles().Files).To(HaveLen(1))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_UploadFiles{
						UploadFiles: &v1.UploadFilesResponse{FilesWritten: 1},
					},
				}
			}()

			resp, err := proxy.UploadFiles(ctx, &v1.UploadFilesRequest{
				WorkingDir: "/home/kvarn/workspace",
				Files: []*v1.FileContent{
					{Path: "test.txt", Content: []byte("hello"), Mode: 0644},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.FilesWritten).To(Equal(int32(1)))
		})
	})

	Describe("ReadFile", func() {
		It("sends read and receives tagged lines", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetReadFile()).NotTo(BeNil())
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_ReadFile{
						ReadFile: &v1.ReadFileResponse{
							Version:    "v1",
							TotalLines: 1,
							Lines:      []*v1.TaggedLine{{Line: 1, Hash: "aa", Content: "content"}},
						},
					},
				}
			}()

			resp, err := proxy.ReadFile(ctx, &v1.ReadFileRequest{
				WorkingDir: "/home/kvarn/workspace",
				Path:       "file.txt",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Version).To(Equal("v1"))
			Expect(resp.Lines).To(HaveLen(1))
		})
	})

	Describe("EditFile", func() {
		It("sends operations and receives updated context", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetEditFile()).NotTo(BeNil())
				Expect(cmd.GetEditFile().ExpectedVersion).To(Equal("v1"))
				Expect(cmd.GetEditFile().Operations).To(HaveLen(1))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_EditFile{
						EditFile: &v1.EditFileResponse{Version: "v2", TotalLines: 1},
					},
				}
			}()

			resp, err := proxy.EditFile(ctx, &v1.EditFileRequest{
				WorkingDir:      "/home/kvarn/workspace",
				Path:            "file.txt",
				ExpectedVersion: "v1",
				Operations: []*v1.EditOperation{
					{Op: v1.EditOp_EDIT_OP_REPLACE, Line: 1, Hash: "aa", Lines: []string{"x"}},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Version).To(Equal("v2"))
		})
	})

	Describe("WriteFile", func() {
		It("sends write and receives version", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetWriteFile()).NotTo(BeNil())
				Expect(cmd.GetWriteFile().Path).To(Equal("file.txt"))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_WriteFile{
						WriteFile: &v1.WriteFileResponse{Version: "v3", TotalLines: 1},
					},
				}
			}()

			resp, err := proxy.WriteFile(ctx, &v1.WriteFileRequest{
				WorkingDir: "/home/kvarn/workspace",
				Path:       "file.txt",
				Content:    []byte("hi\n"),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Version).To(Equal("v3"))
		})
	})

	Describe("CreateSession", func() {
		It("sends create session and receives result", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetCreateSession()).NotTo(BeNil())
				Expect(cmd.GetCreateSession().WorkingDir).To(Equal("/home/kvarn/workspace"))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_CreateSession{
						CreateSession: &v1.CreateSessionResponse{SessionId: "sess-1"},
					},
				}
			}()

			resp, err := proxy.CreateSession(ctx, &v1.CreateSessionRequest{
				WorkingDir: "/home/kvarn/workspace",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.SessionId).To(Equal("sess-1"))
		})
	})

	Describe("SessionExec", func() {
		It("sends session exec and receives result", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetSessionExec()).NotTo(BeNil())
				Expect(cmd.GetSessionExec().SessionId).To(Equal("sess-1"))
				Expect(cmd.GetSessionExec().Command).To(Equal("echo hello"))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_SessionExec{
						SessionExec: &v1.SessionExecResponse{
							ExitCode:   0,
							Stdout:     "hello\n",
							WorkingDir: "/home/kvarn/workspace",
						},
					},
				}
			}()

			resp, err := proxy.SessionExec(ctx, &v1.SessionExecRequest{
				SessionId: "sess-1",
				Command:   "echo hello",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Stdout).To(Equal("hello\n"))
			Expect(resp.WorkingDir).To(Equal("/home/kvarn/workspace"))
		})
	})

	Describe("StreamFromGuest", func() {
		It("receives data written to the transfer pipe before the result is reported", func() {
			var buf bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- proxy.StreamFromGuest(ctx, "/tmp/test-file", &buf)
			}()

			// Simulate runner: receive the UploadFile command, write data
			// to the transfer pipe, then report the result — matching the
			// real runner where ReportResult is sent AFTER the upload RPC
			// completes.
			cmd := <-commandCh
			Expect(cmd.GetUploadFile()).NotTo(BeNil())
			transferID := cmd.GetUploadFile().TransferId

			t, ok := pr.LookupTransfer(transferID)
			Expect(ok).To(BeTrue())

			_, writeErr := t.Writer.Write([]byte("hello from guest"))
			Expect(writeErr).NotTo(HaveOccurred())
			t.Writer.Close()

			resultCh <- &v1.CommandResult{
				CommandId: cmd.CommandId,
				Result: &v1.CommandResult_UploadFileResult{
					UploadFileResult: &v1.FileStreamResult{BytesWritten: 16},
				},
			}

			Expect(<-done).NotTo(HaveOccurred())
			Expect(buf.String()).To(Equal("hello from guest"))
		})

		It("receives large data without deadlocking", func() {
			// Use data larger than any reasonable pipe buffer to ensure the
			// concurrent copy is necessary.
			bigData := bytes.Repeat([]byte("x"), 1024*1024)

			var buf bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- proxy.StreamFromGuest(ctx, "/tmp/big-file", &buf)
			}()

			cmd := <-commandCh
			Expect(cmd.GetUploadFile()).NotTo(BeNil())
			transferID := cmd.GetUploadFile().TransferId

			t, ok := pr.LookupTransfer(transferID)
			Expect(ok).To(BeTrue())

			// Write in chunks like the real handler does.
			for i := 0; i < len(bigData); i += 512 * 1024 {
				end := i + 512*1024
				if end > len(bigData) {
					end = len(bigData)
				}
				_, writeErr := t.Writer.Write(bigData[i:end])
				Expect(writeErr).NotTo(HaveOccurred())
			}
			t.Writer.Close()

			resultCh <- &v1.CommandResult{
				CommandId: cmd.CommandId,
				Result: &v1.CommandResult_UploadFileResult{
					UploadFileResult: &v1.FileStreamResult{BytesWritten: int64(len(bigData))},
				},
			}

			Expect(<-done).NotTo(HaveOccurred())
			Expect(buf.Len()).To(Equal(len(bigData)))
		})

		It("returns error on context cancellation", func() {
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel()

			var buf bytes.Buffer
			err := proxy.StreamFromGuest(cancelCtx, "/tmp/test-file", &buf)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("concurrent tool calls", func() {
		It("delivers results to the correct caller when multiple commands are in flight", func() {
			const numCalls = 5

			// Simulate a runner that processes commands sequentially.
			go func() {
				defer GinkgoRecover()
				for range numCalls {
					cmd := <-commandCh
					resultCh <- &v1.CommandResult{
						CommandId: cmd.CommandId,
						Result: &v1.CommandResult_ReadFile{
							ReadFile: &v1.ReadFileResponse{Version: "v-" + cmd.CommandId},
						},
					}
				}
			}()

			// Launch all callers concurrently.
			type result struct {
				resp *v1.ReadFileResponse
				err  error
			}
			results := make(chan result, numCalls)
			for range numCalls {
				go func() {
					resp, err := proxy.ReadFile(ctx, &v1.ReadFileRequest{
						WorkingDir: "/home/kvarn/workspace",
						Path:       "file.txt",
					})
					results <- result{resp, err}
				}()
			}

			for range numCalls {
				r := <-results
				Expect(r.err).NotTo(HaveOccurred())
				Expect(r.resp.Version).To(HavePrefix("v-cmd-"))
			}
		})

		It("delivers results correctly when mixing Exec and ReadFile", func() {
			go func() {
				defer GinkgoRecover()
				// Process two commands in sequence.
				for range 2 {
					cmd := <-commandCh
					switch cmd.Command.(type) {
					case *v1.RunnerCommand_Exec:
						resultCh <- &v1.CommandResult{
							CommandId: cmd.CommandId,
							Result: &v1.CommandResult_Exec{
								Exec: &v1.ExecResponse{ExitCode: 0, Stdout: "exec-" + cmd.CommandId},
							},
						}
					case *v1.RunnerCommand_ReadFile:
						resultCh <- &v1.CommandResult{
							CommandId: cmd.CommandId,
							Result: &v1.CommandResult_ReadFile{
								ReadFile: &v1.ReadFileResponse{Version: "read-" + cmd.CommandId},
							},
						}
					}
				}
			}()

			execDone := make(chan error, 1)
			readDone := make(chan error, 1)

			go func() {
				resp, err := proxy.Exec(ctx, &v1.ExecRequest{Command: "echo"})
				if err == nil {
					Expect(resp.Stdout).To(HavePrefix("exec-cmd-"))
				}
				execDone <- err
			}()

			go func() {
				resp, err := proxy.ReadFile(ctx, &v1.ReadFileRequest{
					WorkingDir: "/home/kvarn/workspace",
					Path:       "file.txt",
				})
				if err == nil {
					Expect(resp.Version).To(HavePrefix("read-cmd-"))
				}
				readDone <- err
			}()

			Expect(<-execDone).NotTo(HaveOccurred())
			Expect(<-readDone).NotTo(HaveOccurred())
		})
	})

	Describe("CloseSession", func() {
		It("sends close session and receives result", func() {
			go func() {
				cmd := <-commandCh
				Expect(cmd.GetCloseSession()).NotTo(BeNil())
				Expect(cmd.GetCloseSession().SessionId).To(Equal("sess-1"))
				resultCh <- &v1.CommandResult{
					CommandId: cmd.CommandId,
					Result: &v1.CommandResult_CloseSession{
						CloseSession: &v1.CloseSessionResponse{},
					},
				}
			}()

			_, err := proxy.CloseSession(ctx, &v1.CloseSessionRequest{
				SessionId: "sess-1",
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
