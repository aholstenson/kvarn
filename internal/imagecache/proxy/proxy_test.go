package proxy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"

	imageproxy "github.com/aholstenson/kvarn/internal/imagecache/proxy"
	"github.com/aholstenson/kvarn/internal/imagecache/store"
	"github.com/aholstenson/kvarn/internal/imagecache/upstream"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeRegistry is a minimal upstream that serves one repo's manifest+blob and
// counts byte-level traffic so tests can assert "second pull = zero upstream
// bytes".
type fakeRegistry struct {
	mu             *http.ServeMux
	manifest       []byte
	manifestDigest string
	blob           []byte
	blobDigest     string

	manifestHits atomic.Int64
	blobHits     atomic.Int64
}

func newFakeRegistry(repo, tag string, blob []byte) *fakeRegistry {
	blobSum := sha256.Sum256(blob)
	blobDigest := "sha256:" + hex.EncodeToString(blobSum[:])
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"layers":[{"digest":%q}]}`, blobDigest))
	manifestSum := sha256.Sum256(manifest)
	manifestDigest := "sha256:" + hex.EncodeToString(manifestSum[:])
	f := &fakeRegistry{
		manifest:       manifest,
		manifestDigest: manifestDigest,
		blob:           blob,
		blobDigest:     blobDigest,
		mu:             http.NewServeMux(),
	}
	f.mu.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		if path == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		switch {
		case strings.HasPrefix(path, repo+"/manifests/"):
			f.manifestHits.Add(1)
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Write(manifest)
		case strings.HasPrefix(path, repo+"/blobs/"):
			f.blobHits.Add(1)
			w.Header().Set("Docker-Content-Digest", blobDigest)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(blob)))
			w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	})
	return f
}

var _ = Describe("Pull-through proxy", func() {
	var (
		fake *fakeRegistry
		srv  *httptest.Server
		dir  string
		px   *imageproxy.Handler
	)

	BeforeEach(func() {
		fake = newFakeRegistry("library/python", "3.12", []byte("layer-bytes"))
		srv = httptest.NewServer(fake.mu)
		DeferCleanup(srv.Close)

		dir = GinkgoT().TempDir()
		st := store.New(dir)

		u, err := url.Parse(srv.URL)
		Expect(err).NotTo(HaveOccurred())

		upClient := upstream.New()
		upClient.Scheme = "http"
		upClient.Hosts = map[string]string{"docker.io": u.Host}

		px = imageproxy.New(imageproxy.Config{
			Store:          st,
			Upstreams:      []string{"docker.io"},
			UpstreamClient: upClient,
		})
	})

	doGet := func(path string) *http.Response {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		px.ServeHTTP(w, r)
		return w.Result()
	}

	It("answers /v2/ with 200 for API detection", func() {
		resp := doGet("/v2/")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Docker-Distribution-Api-Version")).To(Equal("registry/2.0"))
	})

	It("caches manifests and blobs after the first pull", func() {
		// First manifest fetch — miss, populates cache.
		resp := doGet("/v2/docker.io/library/python/manifests/3.12")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		Expect(body).To(Equal(fake.manifest))
		Expect(fake.manifestHits.Load()).To(Equal(int64(1)))

		// First blob fetch.
		resp = doGet("/v2/docker.io/library/python/blobs/" + fake.blobDigest)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		Expect(body).To(Equal(fake.blob))
		Expect(fake.blobHits.Load()).To(Equal(int64(1)))

		// Second blob fetch — hits cache, upstream not touched again.
		resp = doGet("/v2/docker.io/library/python/blobs/" + fake.blobDigest)
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		Expect(body).To(Equal(fake.blob))
		Expect(fake.blobHits.Load()).To(Equal(int64(1)), "blob cache miss on second pull")
	})

	It("rejects unknown upstreams with a registry-shaped error", func() {
		resp := doGet("/v2/not-configured/lib/manifests/1")
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("rejects malformed blob digests", func() {
		resp := doGet("/v2/docker.io/library/python/blobs/not-a-digest")
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
	})
})
