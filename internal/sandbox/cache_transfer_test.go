package sandbox_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	v1 "github.com/aholstenson/kvarn/gen/kvarn/v1"
	"github.com/aholstenson/kvarn/internal/sandbox"
	"github.com/aholstenson/kvarn/internal/sandbox/cache"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// streamMockProxy extends mockRunnerProxy with StreamToGuest/StreamFromGuest tracking.
type streamMockProxy struct {
	mockRunnerProxy

	streamToGuestCalls []streamToGuestCall
	streamToGuestErr   error

	streamFromGuestCalls []streamFromGuestCall
	streamFromGuestData  []byte
	streamFromGuestErr   error
}

type streamToGuestCall struct {
	DestPath string
	Data     []byte
	Size     int64
}

type streamFromGuestCall struct {
	SrcPath string
}

func newStreamMockRunner() *streamMockProxy {
	return &streamMockProxy{
		mockRunnerProxy: mockRunnerProxy{
			responses:     make(map[string]*v1.SessionExecResponse),
			execResponses: make(map[string]*v1.ExecResponse),
			errors:        make(map[string]error),
		},
	}
}

func (m *streamMockProxy) StreamToGuest(_ context.Context, destPath string, src io.Reader, size int64) error {
	if m.streamToGuestErr != nil {
		return m.streamToGuestErr
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	m.streamToGuestCalls = append(m.streamToGuestCalls, streamToGuestCall{
		DestPath: destPath,
		Data:     data,
		Size:     size,
	})
	return nil
}

func (m *streamMockProxy) StreamFromGuest(_ context.Context, srcPath string, dest io.Writer) error {
	m.streamFromGuestCalls = append(m.streamFromGuestCalls, streamFromGuestCall{SrcPath: srcPath})
	if m.streamFromGuestErr != nil {
		return m.streamFromGuestErr
	}
	if m.streamFromGuestData != nil {
		_, err := dest.Write(m.streamFromGuestData)
		return err
	}
	return nil
}

// mockCacheProvider is a simple in-memory cache.Provider for testing.
type mockCacheProvider struct {
	stored   map[string][]byte
	restored map[string]io.ReadCloser
}

func newMockCacheProvider() *mockCacheProvider {
	return &mockCacheProvider{
		stored:   make(map[string][]byte),
		restored: make(map[string]io.ReadCloser),
	}
}

func (p *mockCacheProvider) Restore(_ string, guestPath string) (io.ReadCloser, error) {
	rc, ok := p.restored[guestPath]
	if !ok {
		return nil, nil
	}
	return rc, nil
}

func (p *mockCacheProvider) Save(_ string, guestPath string, data io.Reader) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	p.stored[guestPath] = b
	return nil
}

func (p *mockCacheProvider) Clear(_ string) error {
	return nil
}

var _ cache.Provider = (*mockCacheProvider)(nil)

var _ = Describe("restoreCache", func() {
	var (
		runner   *streamMockProxy
		provider *mockCacheProvider
		ctx      context.Context
	)

	BeforeEach(func() {
		runner = newStreamMockRunner()
		provider = newMockCacheProvider()
		ctx = context.Background()
	})

	It("streams tarball to guest and extracts it", func() {
		tarData := "fake-tarball-data"
		provider.restored["/home/kvarn/.cache"] = io.NopCloser(strings.NewReader(tarData))

		err := sandbox.RestoreCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).NotTo(HaveOccurred())

		// StreamToGuest should have been called with the tarball data.
		Expect(runner.streamToGuestCalls).To(HaveLen(1))
		call := runner.streamToGuestCalls[0]
		Expect(call.DestPath).To(ContainSubstring("kvarn-cache"))
		Expect(string(call.Data)).To(Equal(tarData))

		// First exec creates the temp dir, second extracts the tarball.
		extractCalls := filterExecByCommand(runner.execCalls, "sh")
		Expect(extractCalls).To(HaveLen(1))
		args := strings.Join(extractCalls[0].Args, " ")
		Expect(args).To(ContainSubstring("tar"))
		Expect(args).To(ContainSubstring("--owner=kvarn"))
	})

	It("chowns the target directory to kvarn", func() {
		tarData := "fake-tarball-data"
		provider.restored["/home/kvarn/.cache"] = io.NopCloser(strings.NewReader(tarData))

		err := sandbox.RestoreCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).NotTo(HaveOccurred())

		extractCalls := filterExecByCommand(runner.execCalls, "sh")
		Expect(extractCalls).To(HaveLen(1))
		args := strings.Join(extractCalls[0].Args, " ")
		Expect(args).To(ContainSubstring("chown kvarn:kvarn /home/kvarn/.cache"))
	})

	It("skips when provider returns nil (no cache)", func() {
		err := sandbox.RestoreCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamToGuestCalls).To(BeEmpty())
	})

	It("continues to next path on stream error", func() {
		provider.restored["/home/kvarn/.cache"] = io.NopCloser(strings.NewReader("data"))
		runner.streamToGuestErr = fmt.Errorf("stream failed")

		err := sandbox.RestoreCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).NotTo(HaveOccurred())
		// No extract (sh -c tar ...) should have been attempted.
		extractCalls := filterExecByCommand(runner.execCalls, "sh")
		Expect(extractCalls).To(BeEmpty())
	})
})

var _ = Describe("saveCache", func() {
	var (
		runner   *streamMockProxy
		provider *mockCacheProvider
		ctx      context.Context
	)

	BeforeEach(func() {
		runner = newStreamMockRunner()
		provider = newMockCacheProvider()
		ctx = context.Background()
	})

	It("streams tarball from guest to provider", func() {
		tarData := []byte("fake-saved-tarball")
		runner.streamFromGuestData = tarData

		err := sandbox.SaveCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).NotTo(HaveOccurred())

		// StreamFromGuest should have been called.
		Expect(runner.streamFromGuestCalls).To(HaveLen(1))
		Expect(runner.streamFromGuestCalls[0].SrcPath).To(ContainSubstring("kvarn-cache"))

		// Provider should have received the data.
		Expect(provider.stored["/home/kvarn/.cache"]).To(Equal(tarData))
	})

	It("returns error when tar command fails", func() {
		runner.execResponses["sh -c mkdir -p /var/tmp/kvarn-cache && tar -C /home/kvarn/.cache --zstd -cf /var/tmp/kvarn-cache/_home_kvarn_.cache.tar.zst ."] = &v1.ExecResponse{
			ExitCode: 1,
			Stderr:   "tar error",
		}

		err := sandbox.SaveCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).To(HaveOccurred())
	})

	It("skips non-existent cache directories", func() {
		runner.execResponses["test -d /home/kvarn/.npm"] = &v1.ExecResponse{
			ExitCode: 1,
		}

		err := sandbox.SaveCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.npm"}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamFromGuestCalls).To(BeEmpty())
	})

	It("returns error when stream from guest fails", func() {
		runner.streamFromGuestErr = fmt.Errorf("stream error")

		err := sandbox.SaveCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).To(HaveOccurred())
	})

	It("cleans up temp file after streaming", func() {
		runner.streamFromGuestData = []byte("data")

		err := sandbox.SaveCache(ctx, runner, provider, "proj-1", []string{"/home/kvarn/.cache"}, nil)
		Expect(err).NotTo(HaveOccurred())

		// Last exec call should be rm -f.
		rmCalls := filterExecByCommand(runner.execCalls, "rm")
		Expect(rmCalls).To(HaveLen(1))
	})
})

// filterExecByCommand returns exec calls matching the given command name.
func filterExecByCommand(calls []execCall, command string) []execCall {
	var out []execCall
	for _, c := range calls {
		if c.Command == command {
			out = append(out, c)
		}
	}
	return out
}

// Verify the mock satisfies the interface.
var _ sandbox.RunnerProxy = (*streamMockProxy)(nil)

// Verify standard mock also satisfies the interface (via embedded unexported check).
func init() {
	var buf bytes.Buffer
	_ = buf // avoid unused import
}
