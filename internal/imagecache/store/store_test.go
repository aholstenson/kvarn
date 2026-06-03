package store_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/imagecache/store"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var _ = Describe("Store", func() {
	var (
		dir string
		s   *store.Store
		now time.Time
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		s = store.New(dir)
		s.Clock = func() time.Time { return now }
	})

	Describe("blobs", func() {
		It("round-trips a payload via digest", func() {
			body := []byte("hello, world")
			d := digestOf(body)

			n, err := s.WriteBlob(d, bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(int64(len(body))))

			r, size, hit, err := s.OpenBlob(d)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(size).To(Equal(int64(len(body))))
			defer r.Close()
			got, err := io.ReadAll(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(body))
		})

		It("misses on an unknown digest", func() {
			d := digestOf([]byte("absent"))
			r, _, hit, err := s.OpenBlob(d)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())
			Expect(r).To(BeNil())
		})

		It("rejects malformed digests", func() {
			_, _, _, err := s.OpenBlob("not-a-digest")
			Expect(err).To(HaveOccurred())
		})

		It("survives concurrent writers of identical bytes", func() {
			body := []byte("racy")
			d := digestOf(body)
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := s.WriteBlob(d, bytes.NewReader(body))
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				Expect(err).NotTo(HaveOccurred())
			}
			_, size, hit, _ := s.OpenBlob(d)
			Expect(hit).To(BeTrue())
			Expect(size).To(Equal(int64(len(body))))
		})
	})

	Describe("manifests", func() {
		const (
			registry = "docker.io"
			name     = "library/python"
			tagRef   = "3.12"
		)
		var (
			body = []byte(`{"schemaVersion":2}`)
			d    = digestOf(body)
		)

		It("stores and retrieves a tag manifest within its TTL", func() {
			err := s.WriteManifest(registry, name, tagRef, body, d, "application/json", "etag-1", true, 5*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			entry, hit, err := s.ReadManifest(registry, name, tagRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(entry.Body).To(Equal(body))
			Expect(entry.Meta.ResolvedDigest).To(Equal(d))
			Expect(entry.Meta.IsTag).To(BeTrue())
			Expect(s.IsManifestFresh(entry.Meta)).To(BeTrue())
		})

		It("treats a tag past its TTL as stale", func() {
			err := s.WriteManifest(registry, name, tagRef, body, d, "application/json", "", true, 5*time.Minute)
			Expect(err).NotTo(HaveOccurred())

			// Advance the clock past the TTL window.
			now = now.Add(10 * time.Minute)
			entry, hit, err := s.ReadManifest(registry, name, tagRef)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(s.IsManifestFresh(entry.Meta)).To(BeFalse())
		})

		It("keeps a digest manifest indefinitely", func() {
			err := s.WriteManifest(registry, name, d, body, d, "application/json", "", false, 0)
			Expect(err).NotTo(HaveOccurred())

			entry, hit, err := s.ReadManifest(registry, name, d)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(s.IsManifestFresh(entry.Meta)).To(BeTrue())

			now = now.Add(365 * 24 * time.Hour)
			entry, _, _ = s.ReadManifest(registry, name, d)
			Expect(s.IsManifestFresh(entry.Meta)).To(BeTrue())
		})

		It("lists every cached manifest", func() {
			Expect(s.WriteManifest(registry, name, tagRef, body, d, "ct", "", true, time.Minute)).To(Succeed())
			Expect(s.WriteManifest(registry, "library/node", "20", body, d, "ct", "", true, time.Minute)).To(Succeed())

			rows, err := s.ListManifests()
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(2))
		})

		It("clears manifests by repo without touching blobs", func() {
			body := []byte("blob-bytes")
			bd := digestOf(body)
			_, err := s.WriteBlob(bd, bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())

			Expect(s.WriteManifest(registry, name, tagRef, body, bd, "ct", "", true, time.Minute)).To(Succeed())
			Expect(s.ClearRepo(name)).To(Succeed())

			_, hit, _ := s.ReadManifest(registry, name, tagRef)
			Expect(hit).To(BeFalse())

			present, _, _ := s.HasBlob(bd)
			Expect(present).To(BeTrue())
		})
	})

	Describe("eviction", func() {
		It("removes the least-recently-used blob first", func() {
			cold := []byte("0000000000000000")
			warm := []byte("1111111111111111")
			cd := digestOf(cold)
			wd := digestOf(warm)

			now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			_, err := s.WriteBlob(cd, bytes.NewReader(cold))
			Expect(err).NotTo(HaveOccurred())

			now = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			_, err = s.WriteBlob(wd, bytes.NewReader(warm))
			Expect(err).NotTo(HaveOccurred())

			// Bump warm access so cold becomes the LRU candidate.
			now = time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
			r, _, _, err := s.OpenBlob(wd)
			Expect(err).NotTo(HaveOccurred())
			r.Close()

			rep, err := s.EvictGlobal(int64(len(warm)))
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.RemovedEntries).To(Equal(1))

			present, _, _ := s.HasBlob(cd)
			Expect(present).To(BeFalse())
			present, _, _ = s.HasBlob(wd)
			Expect(present).To(BeTrue())
		})

		It("is a no-op when usage is already under quota", func() {
			body := []byte("small")
			d := digestOf(body)
			_, err := s.WriteBlob(d, bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())

			rep, err := s.EvictGlobal(int64(len(body)) * 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.RemovedEntries).To(Equal(0))
		})
	})

	Describe("Stats", func() {
		It("counts blob bytes on disk", func() {
			body := []byte("hello")
			d := digestOf(body)
			_, err := s.WriteBlob(d, bytes.NewReader(body))
			Expect(err).NotTo(HaveOccurred())

			st, err := s.Stats()
			Expect(err).NotTo(HaveOccurred())
			Expect(st.BlobCount).To(Equal(1))
			Expect(st.BlobBytes).To(Equal(int64(len(body))))
		})
	})

	It("creates a global .lock file at BaseDir", func() {
		body := []byte("trigger-lock")
		d := digestOf(body)
		_, err := s.WriteBlob(d, bytes.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		_, err = s.EvictGlobal(int64(len(body)) / 2)
		Expect(err).NotTo(HaveOccurred())

		_, statErr := os.Stat(filepath.Join(dir, ".lock"))
		Expect(statErr).NotTo(HaveOccurred())
	})
})
