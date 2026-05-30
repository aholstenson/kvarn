package sandbox_test

import (
	"bytes"
	"context"
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

// mockCacheProvider is an in-memory cache.Provider for transfer tests.
type mockCacheProvider struct {
	restoreData map[string][]byte // keyStr -> exact-hit tar bytes
	warm        map[string]bool   // keyStr -> serve as warm start
	hasKeys     map[string]bool   // keyStr -> Has() == true
	saved       map[string][]byte // keyStr -> bytes received by Save
}

func newMockCacheProvider() *mockCacheProvider {
	return &mockCacheProvider{
		restoreData: make(map[string][]byte),
		warm:        make(map[string]bool),
		hasKeys:     make(map[string]bool),
		saved:       make(map[string][]byte),
	}
}

func keyStr(k cache.Key) string { return k.Bucket + "|" + k.InputKey }

func (p *mockCacheProvider) Restore(k cache.Key) (*cache.RestoreResult, error) {
	d, ok := p.restoreData[keyStr(k)]
	if !ok {
		return nil, nil
	}
	return &cache.RestoreResult{
		Reader:   io.NopCloser(bytes.NewReader(d)),
		Warm:     p.warm[keyStr(k)],
		InputKey: k.InputKey,
	}, nil
}

func (p *mockCacheProvider) Save(k cache.Key, data io.Reader) error {
	b, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	p.saved[keyStr(k)] = b
	return nil
}

func (p *mockCacheProvider) Has(k cache.Key) (bool, error) { return p.hasKeys[keyStr(k)], nil }
func (p *mockCacheProvider) List(string) ([]cache.Entry, error) {
	return nil, nil
}
func (p *mockCacheProvider) Clear(string) error { return nil }
func (p *mockCacheProvider) Evict(cache.Quota) (cache.EvictReport, error) {
	return cache.EvictReport{}, nil
}

var _ cache.Provider = (*mockCacheProvider)(nil)

func layerFor(bucket, inputKey, guestPath string) cache.Layer {
	return cache.Layer{
		Key:       cache.Key{ProjectID: "proj-1", Bucket: bucket, InputKey: inputKey, GuestPath: guestPath},
		GuestPath: guestPath,
	}
}

var _ = Describe("RestoreCache", func() {
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

	It("streams an exact-hit tarball to guest and extracts it", func() {
		l := layerFor("go", "aaaa", "/home/kvarn/go")
		provider.restoreData[keyStr(l.Key)] = []byte("tar-bytes")

		err := sandbox.RestoreCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())

		Expect(runner.streamToGuestCalls).To(HaveLen(1))
		Expect(string(runner.streamToGuestCalls[0].Data)).To(Equal("tar-bytes"))

		extractCalls := filterExecByCommand(runner.execCalls, "sh")
		Expect(extractCalls).To(HaveLen(1))
		args := strings.Join(extractCalls[0].Args, " ")
		Expect(args).To(ContainSubstring("tar"))
		Expect(args).To(ContainSubstring("chown kvarn:kvarn /home/kvarn/go"))
	})

	It("chowns the whole directory chain below the home dir for a nested path", func() {
		l := layerFor("nix-eval", "aaaa", "/home/kvarn/.cache/nix")
		provider.restoreData[keyStr(l.Key)] = []byte("tar-bytes")

		err := sandbox.RestoreCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())

		extractCalls := filterExecByCommand(runner.execCalls, "sh")
		Expect(extractCalls).To(HaveLen(1))
		args := strings.Join(extractCalls[0].Args, " ")
		// The parent .cache must be chowned too, not just the leaf, so the
		// job can later create siblings like .cache/go-build.
		Expect(args).To(ContainSubstring("chown kvarn:kvarn /home/kvarn/.cache /home/kvarn/.cache/nix"))
	})

	It("still restores a warm-start tarball", func() {
		l := layerFor("go", "wanted", "/home/kvarn/go")
		provider.restoreData[keyStr(l.Key)] = []byte("warm-bytes")
		provider.warm[keyStr(l.Key)] = true

		err := sandbox.RestoreCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamToGuestCalls).To(HaveLen(1))
		Expect(string(runner.streamToGuestCalls[0].Data)).To(Equal("warm-bytes"))
	})

	It("skips on a miss", func() {
		l := layerFor("go", "aaaa", "/home/kvarn/go")
		err := sandbox.RestoreCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamToGuestCalls).To(BeEmpty())
	})
})

var _ = Describe("SaveCache", func() {
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

	It("streams a tarball from guest to the provider", func() {
		l := layerFor("go", "aaaa", "/home/kvarn/go")
		runner.streamFromGuestData = []byte("saved-tar")

		err := sandbox.SaveCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamFromGuestCalls).To(HaveLen(1))
		Expect(provider.saved[keyStr(l.Key)]).To(Equal([]byte("saved-tar")))
	})

	It("skips the guest-side tar entirely when the layer is already present", func() {
		l := layerFor("go", "aaaa", "/home/kvarn/go")
		provider.hasKeys[keyStr(l.Key)] = true

		err := sandbox.SaveCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamFromGuestCalls).To(BeEmpty())
		Expect(filterExecByCommand(runner.execCalls, "sh")).To(BeEmpty())
		Expect(provider.saved).To(BeEmpty())
	})

	It("skips non-existent cache directories", func() {
		l := layerFor("npm", "aaaa", "/home/kvarn/.npm")
		runner.execResponses["test -d /home/kvarn/.npm"] = &v1.ExecResponse{ExitCode: 1}

		err := sandbox.SaveCache(ctx, runner, provider, []cache.Layer{l}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.streamFromGuestCalls).To(BeEmpty())
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
