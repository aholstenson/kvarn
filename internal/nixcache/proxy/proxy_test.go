package proxy_test

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aholstenson/kvarn/internal/nixcache/nixbase32"
	nixproxy "github.com/aholstenson/kvarn/internal/nixcache/proxy"
	"github.com/aholstenson/kvarn/internal/nixcache/store"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	storeHash       = "0mdqa9w1p6cmli6976v4wi0sw9r4p5pr"
	secondStoreHash = "2mdqa9w1p6cmli6976v4wi0sw9r4p5pr"
)

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return nixbase32.Encode(sum[:])
}

// fakeCache is a minimal upstream binary cache serving one store path. It
// counts requests so tests can assert that a second pull never reaches it.
type fakeCache struct {
	mux     *http.ServeMux
	narinfo []byte
	nar     []byte
	narName string

	// narBody, when set, is served instead of nar under narName, which is how
	// a test hands out bytes the narinfo did not describe.
	narBody []byte

	cacheInfoHits atomic.Int64
	narinfoHits   atomic.Int64
	narHits       atomic.Int64
	lastRange     atomic.Value
	failNarinfo   atomic.Bool
}

func newFakeCache(nar []byte) *fakeCache {
	return newFakeCacheAt(storeHash, nar)
}

// newFakeCacheAt serves one store path the way cache.nixos.org does: the NAR
// file is named after the uncompressed archive while what travels is the
// compressed file, so the name never covers the bytes on the wire and the
// narinfo's FileHash is the only thing that describes them.
func newFakeCacheAt(storePathHash string, nar []byte) *fakeCache {
	archive := append([]byte("uncompressed:"), nar...)
	name := hashOf(archive) + ".nar.zst"
	f := &fakeCache{
		mux:     http.NewServeMux(),
		nar:     nar,
		narName: name,
		narinfo: []byte(fmt.Sprintf(
			"StorePath: /nix/store/%s-hello\nURL: nar/%s\nCompression: zstd\nFileHash: sha256:%s\nFileSize: %d\nNarHash: sha256:%s\nNarSize: %d\n",
			storePathHash, name, hashOf(nar), len(nar), hashOf(archive), len(archive))),
	}
	f.lastRange.Store("")
	f.mux.HandleFunc("/nix-cache-info", func(w http.ResponseWriter, r *http.Request) {
		f.cacheInfoHits.Add(1)
		w.Write([]byte("StoreDir: /nix/store\nWantMassQuery: 1\nPriority: 40\n"))
	})
	f.mux.HandleFunc("/"+storePathHash+".narinfo", func(w http.ResponseWriter, r *http.Request) {
		f.narinfoHits.Add(1)
		if f.failNarinfo.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(f.narinfo)
	})
	f.mux.HandleFunc("/nar/"+name, func(w http.ResponseWriter, r *http.Request) {
		f.narHits.Add(1)
		f.lastRange.Store(r.Header.Get("Range"))
		body := f.nar
		if f.narBody != nil {
			body = f.narBody
		}
		http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(body)))
	})
	return f
}

var _ = Describe("Pull-through Nix cache", func() {
	var (
		fake *fakeCache
		srv  *httptest.Server
		st   *store.Store
		px   *nixproxy.Handler
		host string
	)

	BeforeEach(func() {
		fake = newFakeCache([]byte("nar-bytes-of-hello"))
		srv = httptest.NewServer(fake.mux)
		DeferCleanup(srv.Close)

		u, err := url.Parse(srv.URL)
		Expect(err).NotTo(HaveOccurred())
		host = u.Hostname()

		st = store.New(GinkgoT().TempDir())
		px, err = nixproxy.New(nixproxy.Config{
			Store:     st,
			Upstreams: []string{srv.URL},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	do := func(method, path string, hdr ...string) *http.Response {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		px.ServeHTTP(w, r)
		return w.Result()
	}
	body := func(resp *http.Response) string {
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return string(b)
	}
	// fetchNarInfo does what Nix does before it downloads an archive: ask for
	// the record describing it. That is where the handler learns what the
	// download must hash to.
	fetchNarInfo := func(storePathHash string) {
		resp := do(http.MethodGet, "/"+host+"/"+storePathHash+".narinfo")
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		body(resp)
	}

	Describe("nix-cache-info", func() {
		It("reports a priority ahead of the upstream and fetches the record once", func() {
			resp := do(http.MethodGet, "/"+host+"/nix-cache-info")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			got := body(resp)
			Expect(got).To(ContainSubstring("StoreDir: /nix/store\n"))
			Expect(got).To(ContainSubstring("WantMassQuery: 1\n"))
			Expect(got).To(ContainSubstring("Priority: 10\n"))
			Expect(got).NotTo(ContainSubstring("Priority: 40"))

			resp = do(http.MethodGet, "/"+host+"/nix-cache-info")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(fake.cacheInfoHits.Load()).To(Equal(int64(1)))
		})

		It("reports an unreachable upstream as a gateway error", func() {
			srv.Close()
			resp := do(http.MethodGet, "/"+host+"/nix-cache-info")
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
		})
	})

	Describe("narinfo", func() {
		It("serves from the store after the first fetch", func() {
			resp := do(http.MethodGet, "/"+host+"/"+storeHash+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Type")).To(Equal("text/x-nix-narinfo"))
			Expect(body(resp)).To(Equal(string(fake.narinfo)))

			resp = do(http.MethodGet, "/"+host+"/"+storeHash+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(body(resp)).To(Equal(string(fake.narinfo)))
			Expect(fake.narinfoHits.Load()).To(Equal(int64(1)))
		})

		It("answers HEAD by fetching the record, so the GET after it is a hit", func() {
			resp := do(http.MethodHead, "/"+host+"/"+storeHash+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Length")).To(Equal(fmt.Sprint(len(fake.narinfo))))
			Expect(body(resp)).To(BeEmpty())

			resp = do(http.MethodGet, "/"+host+"/"+storeHash+".narinfo")
			Expect(body(resp)).To(Equal(string(fake.narinfo)))
			Expect(fake.narinfoHits.Load()).To(Equal(int64(1)))
		})

		It("remembers a miss so the next job does not ask upstream again", func() {
			missing := "1mdqa9w1p6cmli6976v4wi0sw9r4p5pr"
			resp := do(http.MethodGet, "/"+host+"/"+missing+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
			resp = do(http.MethodGet, "/"+host+"/"+missing+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
			// The fake has no handler for this hash, so a second upstream
			// request would show up as a second 404 from the mux; count via
			// the narinfo handler staying untouched and the store staying
			// empty instead.
			stats, err := st.Stats()
			Expect(err).NotTo(HaveOccurred())
			Expect(stats.NarInfoCount).To(BeZero())
		})

		It("does not store an upstream error and reports it as a gateway error", func() {
			fake.failNarinfo.Store(true)
			resp := do(http.MethodGet, "/"+host+"/"+storeHash+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))

			fake.failNarinfo.Store(false)
			resp = do(http.MethodGet, "/"+host+"/"+storeHash+".narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(body(resp)).To(Equal(string(fake.narinfo)))
		})

		It("rejects a hash that is not a store path hash", func() {
			resp := do(http.MethodGet, "/"+host+"/not-a-hash.narinfo")
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})
	})

	Describe("NAR files", func() {
		It("caches the file on the first download and serves the second locally", func() {
			fetchNarInfo(storeHash)
			resp := do(http.MethodGet, "/"+host+"/nar/"+fake.narName)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Type")).To(Equal("application/x-nix-nar"))
			Expect(resp.Header.Get("Content-Length")).To(Equal(fmt.Sprint(len(fake.nar))))
			Expect(body(resp)).To(Equal(string(fake.nar)))

			resp = do(http.MethodGet, "/"+host+"/nar/"+fake.narName)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(body(resp)).To(Equal(string(fake.nar)))
			Expect(fake.narHits.Load()).To(Equal(int64(1)), "second download reached upstream")
		})

		It("honours a range request against a cached file", func() {
			fetchNarInfo(storeHash)
			body(do(http.MethodGet, "/"+host+"/nar/"+fake.narName))

			resp := do(http.MethodGet, "/"+host+"/nar/"+fake.narName, "Range", "bytes=4-")
			Expect(resp.StatusCode).To(Equal(http.StatusPartialContent))
			Expect(body(resp)).To(Equal(string(fake.nar[4:])))
			Expect(fake.narHits.Load()).To(Equal(int64(1)))
		})

		It("passes a range request for an uncached file through to upstream", func() {
			resp := do(http.MethodGet, "/"+host+"/nar/"+fake.narName, "Range", "bytes=4-")
			Expect(resp.StatusCode).To(Equal(http.StatusPartialContent))
			Expect(body(resp)).To(Equal(string(fake.nar[4:])))
			Expect(fake.lastRange.Load()).To(Equal("bytes=4-"))

			// A partial transfer cannot fill the cache.
			_, _, hit, err := st.OpenNar(fake.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())
		})

		It("answers HEAD for an uncached file from upstream without caching it", func() {
			resp := do(http.MethodHead, "/"+host+"/nar/"+fake.narName)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Length")).To(Equal(fmt.Sprint(len(fake.nar))))
			_, _, hit, err := st.OpenNar(fake.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())
		})

		It("still streams bytes the narinfo did not describe, but does not keep them", func() {
			fetchNarInfo(storeHash)
			fake.narBody = []byte("corrupted-transfer")
			resp := do(http.MethodGet, "/"+host+"/nar/"+fake.narName)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(body(resp)).To(Equal("corrupted-transfer"))

			_, _, hit, err := st.OpenNar(fake.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())

			fake.narBody = nil
			resp = do(http.MethodGet, "/"+host+"/nar/"+fake.narName)
			Expect(body(resp)).To(Equal(string(fake.nar)))
			Expect(fake.narHits.Load()).To(Equal(int64(2)))
		})

		It("streams a file no narinfo has named without keeping it", func() {
			resp := do(http.MethodGet, "/"+host+"/nar/"+fake.narName)
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(body(resp)).To(Equal(string(fake.nar)))

			_, _, hit, err := st.OpenNar(fake.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())
		})

		It("caches a file a stored narinfo names, without asking upstream for the record again", func() {
			fetchNarInfo(storeHash)
			body(do(http.MethodGet, "/"+host+"/nar/"+fake.narName))

			// A second handler over the same store stands in for a restarted
			// orchestrator: the record comes back from disk, and that is
			// enough to verify a download it has never seen.
			Expect(st.Clear()).To(Succeed())
			fresh, err := nixproxy.New(nixproxy.Config{Store: st, Upstreams: []string{srv.URL}})
			Expect(err).NotTo(HaveOccurred())
			Expect(st.WriteNarInfo(host, storeHash, fake.narinfo)).To(Succeed())

			w := httptest.NewRecorder()
			fresh.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+host+"/"+storeHash+".narinfo", nil))
			Expect(w.Result().StatusCode).To(Equal(http.StatusOK))

			w = httptest.NewRecorder()
			fresh.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+host+"/nar/"+fake.narName, nil))
			Expect(w.Result().StatusCode).To(Equal(http.StatusOK))

			_, _, hit, err := st.OpenNar(fake.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(fake.narinfoHits.Load()).To(Equal(int64(1)))
		})

		It("reports an absent file as 404", func() {
			other := newFakeCache([]byte("some other content"))
			resp := do(http.MethodGet, "/"+host+"/nar/"+other.narName)
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})

		It("rejects a name that is not hash.nar", func() {
			resp := do(http.MethodGet, "/"+host+"/nar/../../etc/passwd")
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})

		It("sweeps the least recently used file once the quota is exceeded", func() {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			st.Clock = func() time.Time { return now }
			var err error
			px, err = nixproxy.New(nixproxy.Config{
				Store:            st,
				Upstreams:        []string{srv.URL},
				GlobalQuotaBytes: int64(len(fake.nar)) + 4,
			})
			Expect(err).NotTo(HaveOccurred())

			fetchNarInfo(storeHash)
			body(do(http.MethodGet, "/"+host+"/nar/"+fake.narName))
			now = now.Add(time.Minute)

			second := newFakeCacheAt(secondStoreHash, []byte("a second file, larger"))
			fake.mux.Handle("/nar/"+second.narName, second.mux)
			fake.mux.Handle("/"+secondStoreHash+".narinfo", second.mux)
			fetchNarInfo(secondStoreHash)
			body(do(http.MethodGet, "/"+host+"/nar/"+second.narName))

			_, _, hit, err := st.OpenNar(fake.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse(), "oldest file survived the sweep")
			_, _, hit, err = st.OpenNar(second.narName)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
		})
	})

	Describe("routing", func() {
		It("rejects an upstream that is not configured", func() {
			resp := do(http.MethodGet, "/other.example/nix-cache-info")
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
		})

		It("rejects writes", func() {
			w := httptest.NewRecorder()
			px.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/"+host+"/nar/x.nar", strings.NewReader("x")))
			Expect(w.Result().StatusCode).To(Equal(http.StatusMethodNotAllowed))
		})

		It("refuses an upstream without a hostname", func() {
			_, err := nixproxy.New(nixproxy.Config{Store: st, Upstreams: []string{"cache.nixos.org"}})
			Expect(err).To(HaveOccurred())
		})

		It("exposes the hostnames it routes by", func() {
			Expect(px.UpstreamHosts()).To(ConsistOf(host))
		})
	})
})
