package preview_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/preview"
	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/sandbox"
)

// guestStub answers the scripts a capture and a restore run, recording what
// crossed it so a spec can assert on the order as well as the content.
type guestStub struct {
	mu sync.Mutex
	// events is everything that happened, in order.
	events []string
	// scripts are the privileged shell scripts, verbatim.
	scripts []string
	// shell are the commands run in the boot's shell session.
	shell []string

	// hasState decides what the state probe answers.
	hasState bool
	// nothingToTar makes the archiving script report an empty guest.
	nothingToTar bool
	// tarExit, when non-zero, fails the archiving script.
	tarExit int32
	// shellExit, when non-zero, fails every shell command.
	shellExit int32

	// archive is what the guest hands back when its archive is streamed out.
	archive []byte
	// received is what was streamed into the guest.
	received []byte
	// stopped are the process IDs StopProcess was asked for.
	stopped []string
}

func newGuestStub() *guestStub {
	return &guestStub{archive: []byte("tarball")}
}

func (g *guestStub) note(event string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, event)
}

func (g *guestStub) Exec(_ context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	script := strings.Join(req.Args, " ")
	g.mu.Lock()
	g.scripts = append(g.scripts, script)
	g.mu.Unlock()

	switch {
	case req.Command == "rm":
		g.note("cleanup")
		return &v1.ExecResponse{}, nil
	case strings.Contains(script, "ls -A"):
		g.note("probe")
		if g.hasState {
			return &v1.ExecResponse{}, nil
		}
		return &v1.ExecResponse{ExitCode: 1}, nil
	case strings.Contains(script, "--zstd -cf"):
		g.note("tar")
		if g.nothingToTar {
			return &v1.ExecResponse{ExitCode: 3}, nil
		}
		return &v1.ExecResponse{ExitCode: g.tarExit, Stderr: "tar: no space left"}, nil
	case strings.Contains(script, "--zstd -xf"):
		g.note("untar")
		return &v1.ExecResponse{}, nil
	case strings.Contains(script, "chown kvarn:kvarn"):
		g.note("prepare")
		return &v1.ExecResponse{}, nil
	default:
		g.note("sh")
		return &v1.ExecResponse{}, nil
	}
}

func (g *guestStub) SessionExec(_ context.Context, req *v1.SessionExecRequest, _ sandbox.OutputCallback) (*v1.SessionExecResponse, error) {
	g.mu.Lock()
	g.shell = append(g.shell, req.Command)
	g.mu.Unlock()
	g.note("shell:" + req.Command)
	return &v1.SessionExecResponse{ExitCode: g.shellExit, Stderr: "hook failed"}, nil
}

func (g *guestStub) StreamToGuest(_ context.Context, _ string, src io.Reader, _ int64) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.received = data
	g.mu.Unlock()
	g.note("stream-in")
	return nil
}

func (g *guestStub) StreamFromGuest(_ context.Context, _ string, dest io.Writer) error {
	g.note("stream-out")
	_, err := io.Copy(dest, bytes.NewReader(g.archive))
	return err
}

func (g *guestStub) StartProcess(context.Context, *v1.StartProcessRequest, sandbox.OutputCallback, sandbox.ProcessExitCallback) (*v1.StartProcessResponse, error) {
	return nil, errors.ErrUnsupported
}

func (g *guestStub) StopProcess(_ context.Context, req *v1.StopProcessRequest) (*v1.StopProcessResponse, error) {
	g.mu.Lock()
	g.stopped = append(g.stopped, req.ProcessId)
	g.mu.Unlock()
	g.note("stop:" + req.ProcessId)
	return &v1.StopProcessResponse{}, nil
}

func (g *guestStub) ListProcesses(context.Context, *v1.ListProcessesRequest) (*v1.ListProcessesResponse, error) {
	return nil, errors.ErrUnsupported
}

func (g *guestStub) CreateSession(context.Context, *v1.CreateSessionRequest) (*v1.CreateSessionResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestStub) CloseSession(context.Context, *v1.CloseSessionRequest) (*v1.CloseSessionResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestStub) UploadFiles(context.Context, *v1.UploadFilesRequest) (*v1.UploadFilesResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestStub) ReadFile(context.Context, *v1.ReadFileRequest) (*v1.ReadFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestStub) EditFile(context.Context, *v1.EditFileRequest) (*v1.EditFileResponse, error) {
	return nil, errors.ErrUnsupported
}
func (g *guestStub) WriteFile(context.Context, *v1.WriteFileRequest) (*v1.WriteFileResponse, error) {
	return nil, errors.ErrUnsupported
}

func (g *guestStub) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.events...)
}

func (g *guestStub) allScripts() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.scripts, "\n---\n")
}

var _ = Describe("Preview state", func() {
	var (
		ctx   context.Context
		guest *guestStub
		store *snapshot.FileStore
		id    snapshot.ID
	)

	BeforeEach(func() {
		ctx = context.Background()
		guest = newGuestStub()
		store = snapshot.NewFileStore(GinkgoT().TempDir())
		id = snapshot.ID{ProjectID: "proj", RefLabel: "main"}
	})

	Describe("PrepareStateDir", func() {
		It("creates the state directory and gives it to the kvarn user", func() {
			Expect(preview.PrepareStateDir(ctx, guest)).To(Succeed())
			Expect(guest.allScripts()).To(ContainSubstring("mkdir -p '/home/kvarn/state'"))
			Expect(guest.allScripts()).To(ContainSubstring("chown kvarn:kvarn '/home/kvarn/state'"))
		})
	})

	Describe("HasState", func() {
		It("reports an empty state directory as nothing to keep", func() {
			has, err := preview.HasState(ctx, guest, project.PreviewState{})
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeFalse())
		})

		It("reports a state directory with something in it", func() {
			guest.hasState = true
			has, err := preview.HasState(ctx, guest, project.PreviewState{})
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeTrue())
		})

		It("also looks at the declared paths", func() {
			_, err := preview.HasState(ctx, guest, project.PreviewState{
				Paths: []string{"/home/kvarn/pgdata"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(guest.allScripts()).To(ContainSubstring("'/home/kvarn/pgdata'"))
		})
	})

	Describe("StopServices", func() {
		var cfg *project.Config

		BeforeEach(func() {
			cfg = &project.Config{Preview: project.Preview{
				Sites: map[string]project.PreviewSite{"web": {Port: 3000}},
				Serve: []project.PreviewProcess{
					{Name: "database", Run: "postgres"},
					{Name: "web", Run: "npm start"},
				},
			}}
		})

		It("stops each serve step, last started first", func() {
			Expect(preview.StopServices(ctx, guest, cfg, "proj/main", 0)).To(Succeed())
			Expect(guest.stopped).To(Equal([]string{"proj/main/serve-1", "proj/main/serve-0"}))
		})

		It("has nothing to stop when the repository declares no serve steps", func() {
			bare := &project.Config{Preview: project.Preview{
				Sites: map[string]project.PreviewSite{"web": {Port: 3000}},
			}}
			Expect(preview.StopServices(ctx, guest, bare, "proj/main", 0)).To(Succeed())
			Expect(guest.stopped).To(BeEmpty())
		})

		It("has nothing to stop on a sandbox that supervises nothing", func() {
			Expect(preview.StopServices(ctx, nil, cfg, "proj/main", 0)).To(Succeed())
		})
	})

	Describe("Capture", func() {
		opts := func(guest *guestStub, store snapshot.Store, id snapshot.ID, st project.PreviewState) preview.CaptureOpts {
			return preview.CaptureOpts{
				Proxy: guest, Runner: guest, ShellSessionID: "shell",
				Store: store, ID: id, State: st,
			}
		}

		It("archives the state directory and stores it", func() {
			meta, err := preview.Capture(ctx, opts(guest, store, id, project.PreviewState{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Bytes).To(Equal(int64(len("tarball"))))

			r, _, err := store.Open(id)
			Expect(err).NotTo(HaveOccurred())
			defer r.Close()
			body, err := io.ReadAll(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).To(Equal("tarball"))
		})

		It("archives the declared paths alongside the state directory", func() {
			_, err := preview.Capture(ctx, opts(guest, store, id, project.PreviewState{
				Paths: []string{"/home/kvarn/pgdata"},
			}))
			Expect(err).NotTo(HaveOccurred())
			// Paths go into the archive relative to /, so one tar can hold state
			// from anywhere in the filesystem and put it all back where it was.
			Expect(guest.allScripts()).To(ContainSubstring("'home/kvarn/pgdata'"))
			Expect(guest.allScripts()).To(ContainSubstring("'home/kvarn/state'"))
		})

		It("runs the save hooks before anything is archived", func() {
			_, err := preview.Capture(ctx, opts(guest, store, id, project.PreviewState{
				Save: []project.Step{{Name: "Dump database", Run: "pg_dump > $KVARN_PREVIEW_STATE_DIR/app.dump"}},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(guest.seen()).To(Equal([]string{
				"shell:pg_dump > $KVARN_PREVIEW_STATE_DIR/app.dump",
				"tar", "stream-out", "cleanup",
			}))
		})

		It("stores nothing when a save hook fails", func() {
			guest.shellExit = 1
			_, err := preview.Capture(ctx, opts(guest, store, id, project.PreviewState{
				Save: []project.Step{{Name: "Dump database", Run: "pg_dump"}},
			}))
			Expect(err).To(MatchError(ContainSubstring(`state save step "Dump database"`)))

			_, _, err = store.Open(id)
			Expect(err).To(MatchError(snapshot.ErrNoSnapshot))
		})

		It("stores nothing when the guest holds none of the declared paths", func() {
			guest.nothingToTar = true
			meta, err := preview.Capture(ctx, opts(guest, store, id, project.PreviewState{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.CreatedAt.IsZero()).To(BeTrue())

			_, _, err = store.Open(id)
			Expect(err).To(MatchError(snapshot.ErrNoSnapshot))
		})

		It("reports a tar that failed rather than storing a partial archive", func() {
			guest.tarExit = 2
			_, err := preview.Capture(ctx, opts(guest, store, id, project.PreviewState{}))
			Expect(err).To(MatchError(ContainSubstring("archive preview state")))

			_, _, err = store.Open(id)
			Expect(err).To(MatchError(snapshot.ErrNoSnapshot))
		})

		It("refuses an archive over its cap and keeps the previous one", func() {
			Expect(store.Save(id, snapshot.Meta{}, strings.NewReader("older"))).To(Succeed())

			guest.archive = bytes.Repeat([]byte("x"), 4096)
			o := opts(guest, store, id, project.PreviewState{})
			o.MaxBytes = 16
			_, err := preview.Capture(ctx, o)
			Expect(err).To(MatchError(preview.ErrStateTooLarge))

			// The cap has to be enforced before the rename, or refusing the capture
			// would itself be what loses the data.
			r, _, err := store.Open(id)
			Expect(err).NotTo(HaveOccurred())
			defer r.Close()
			body, err := io.ReadAll(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).To(Equal("older"))
		})

		It("records the commit and hostnames the archive came from", func() {
			o := opts(guest, store, id, project.PreviewState{})
			o.Meta = snapshot.Meta{Commit: "abc1234", Hosts: []string{"pr-9.preview.example.com"}, Ref: "feat/login"}
			_, err := preview.Capture(ctx, o)
			Expect(err).NotTo(HaveOccurred())

			meta, err := store.Stat(id)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Commit).To(Equal("abc1234"))
			Expect(meta.Hosts).To(ConsistOf("pr-9.preview.example.com"))
			Expect(meta.Ref).To(Equal("feat/login"))
		})
	})

	Describe("Restore", func() {
		opts := func(st project.PreviewState) preview.RestoreOpts {
			return preview.RestoreOpts{
				Proxy: guest, Runner: guest, ShellSessionID: "shell",
				Store: store, ID: id, State: st,
			}
		}

		It("reports nothing restored on the first boot of a preview", func() {
			restored, err := preview.Restore(ctx, opts(project.PreviewState{}))
			Expect(err).NotTo(HaveOccurred())
			Expect(restored).To(BeFalse())
			Expect(guest.seen()).To(BeEmpty())
		})

		It("streams the archive in, unpacks it, then runs the restore hooks", func() {
			Expect(store.Save(id, snapshot.Meta{}, strings.NewReader("tarball"))).To(Succeed())

			restored, err := preview.Restore(ctx, opts(project.PreviewState{
				Restore: []project.Step{{Name: "Load database", Run: "pg_restore app.dump"}},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(restored).To(BeTrue())
			Expect(guest.seen()).To(Equal([]string{
				"sh", "stream-in", "untar", "shell:pg_restore app.dump",
			}))
			Expect(string(guest.received)).To(Equal("tarball"))
		})

		It("does not run the restore hooks when there was nothing to restore", func() {
			restored, err := preview.Restore(ctx, opts(project.PreviewState{
				Restore: []project.Step{{Name: "Load database", Run: "pg_restore app.dump"}},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(restored).To(BeFalse())
			Expect(guest.shell).To(BeEmpty())
		})

		It("chowns every directory the privileged mkdir created", func() {
			Expect(store.Save(id, snapshot.Meta{}, strings.NewReader("tarball"))).To(Succeed())

			_, err := preview.Restore(ctx, opts(project.PreviewState{
				Paths: []string{"/home/kvarn/.local/share/containers/storage/volumes/pgdata"},
			}))
			Expect(err).NotTo(HaveOccurred())
			// Otherwise the ancestors stay root-owned and the preview's own writes
			// into them are denied.
			Expect(guest.allScripts()).To(ContainSubstring("'/home/kvarn/.local'"))
			Expect(guest.allScripts()).To(ContainSubstring("'/home/kvarn/.local/share'"))
		})

		It("stamps the archive so restoring holds off the prune horizon", func() {
			Expect(store.Save(id, snapshot.Meta{}, strings.NewReader("tarball"))).To(Succeed())

			report, err := store.Prune(time.Nanosecond, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(Equal(1), "the archive should have been old enough to sweep")

			Expect(store.Save(id, snapshot.Meta{}, strings.NewReader("tarball"))).To(Succeed())
			_, err = preview.Restore(ctx, opts(project.PreviewState{}))
			Expect(err).NotTo(HaveOccurred())

			report, err = store.Prune(time.Hour, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(BeZero())
		})

		It("fails the boot when a restore hook fails, rather than coming up empty", func() {
			Expect(store.Save(id, snapshot.Meta{}, strings.NewReader("tarball"))).To(Succeed())
			guest.shellExit = 1

			_, err := preview.Restore(ctx, opts(project.PreviewState{
				Restore: []project.Step{{Name: "Load database", Run: "pg_restore"}},
			}))
			Expect(err).To(MatchError(ContainSubstring(`state restore step "Load database"`)))
		})
	})
})
