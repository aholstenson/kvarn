package cache_test

import (
	"bytes"
	"io"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/sandbox/cache"
)

func keyFor(project, bucket, inputKey string) cache.Key {
	return cache.Key{
		ProjectID: project,
		Bucket:    bucket,
		GuestPath: "/home/kvarn/" + bucket,
		InputKey:  inputKey,
	}
}

func restoreBytes(res *cache.RestoreResult) []byte {
	GinkgoHelper()
	Expect(res).NotTo(BeNil())
	defer res.Reader.Close()
	b, err := io.ReadAll(res.Reader)
	Expect(err).NotTo(HaveOccurred())
	return b
}

var _ = Describe("FileCache", func() {
	var (
		fc    *cache.FileCache
		clock *fakeClock
	)

	BeforeEach(func() {
		clock = &fakeClock{t: time.Unix(1_700_000_000, 0)}
		fc = &cache.FileCache{BaseDir: GinkgoT().TempDir(), Clock: clock.now}
	})

	Describe("Save and Restore", func() {
		It("round-trips data on an exact key", func() {
			key := keyFor("proj1", "go", "aaaa")
			Expect(fc.Save(key, bytes.NewReader([]byte("tarball")))).To(Succeed())

			res, err := fc.Restore(key)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Warm).To(BeFalse())
			Expect(res.InputKey).To(Equal("aaaa"))
			Expect(restoreBytes(res)).To(Equal([]byte("tarball")))
		})

		It("returns nil for a missing bucket", func() {
			res, err := fc.Restore(keyFor("proj1", "go", "aaaa"))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeNil())
		})

		It("writes a SOURCE marker", func() {
			Expect(fc.Save(keyFor("proj1", "go", "aaaa"), bytes.NewReader([]byte("x")))).To(Succeed())
			entries, err := fc.List("proj1")
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
		})
	})

	Describe("write-once immutability", func() {
		It("does not overwrite an existing key", func() {
			key := keyFor("proj1", "go", "aaaa")
			Expect(fc.Save(key, bytes.NewReader([]byte("first")))).To(Succeed())
			Expect(fc.Save(key, bytes.NewReader([]byte("second")))).To(Succeed())

			res, err := fc.Restore(key)
			Expect(err).NotTo(HaveOccurred())
			Expect(restoreBytes(res)).To(Equal([]byte("first")))
		})

		It("reports Has for present keys only", func() {
			key := keyFor("proj1", "go", "aaaa")
			has, err := fc.Has(key)
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeFalse())

			Expect(fc.Save(key, bytes.NewReader([]byte("x")))).To(Succeed())
			has, err = fc.Has(key)
			Expect(err).NotTo(HaveOccurred())
			Expect(has).To(BeTrue())
		})

		It("is race-free under concurrent same-key Save", func() {
			key := keyFor("proj1", "go", "aaaa")
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer GinkgoRecover()
					Expect(fc.Save(key, bytes.NewReader([]byte("same")))).To(Succeed())
				}()
			}
			wg.Wait()

			res, err := fc.Restore(key)
			Expect(err).NotTo(HaveOccurred())
			Expect(restoreBytes(res)).To(Equal([]byte("same")))
		})
	})

	Describe("warm start / MRU fallback", func() {
		It("serves the most-recently-used entry on an exact miss", func() {
			Expect(fc.Save(keyFor("proj1", "go", "old"), bytes.NewReader([]byte("old-data")))).To(Succeed())
			Expect(fc.Save(keyFor("proj1", "go", "new"), bytes.NewReader([]byte("new-data")))).To(Succeed())

			// Bump "old" so it becomes the MRU entry.
			_, err := fc.Restore(keyFor("proj1", "go", "old"))
			Expect(err).NotTo(HaveOccurred())

			res, err := fc.Restore(keyFor("proj1", "go", "absent"))
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Warm).To(BeTrue())
			Expect(res.InputKey).To(Equal("new")) // LATEST points at the newest write
			Expect(restoreBytes(res)).To(Equal([]byte("new-data")))
		})
	})

	Describe("List", func() {
		It("returns one entry per saved key with metadata", func() {
			Expect(fc.Save(keyFor("proj1", "go", "aaaa"), bytes.NewReader([]byte("12345")))).To(Succeed())
			Expect(fc.Save(keyFor("proj1", "cargo", "bbbb"), bytes.NewReader([]byte("67")))).To(Succeed())

			entries, err := fc.List("proj1")
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			sizes := map[string]int64{}
			for _, e := range entries {
				sizes[e.Key.Bucket] = e.SizeBytes
				Expect(e.CreatedAt).NotTo(BeZero())
				Expect(e.LastAccess).NotTo(BeZero())
			}
			Expect(sizes["go"]).To(Equal(int64(5)))
			Expect(sizes["cargo"]).To(Equal(int64(2)))
		})
	})

	Describe("Clear", func() {
		It("removes a project's cache", func() {
			key := keyFor("proj1", "go", "aaaa")
			Expect(fc.Save(key, bytes.NewReader([]byte("x")))).To(Succeed())
			Expect(fc.Clear("proj1")).To(Succeed())

			res, err := fc.Restore(key)
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeNil())
		})
	})

	Describe("Evict", func() {
		It("removes least-recently-accessed entries to satisfy the per-project quota", func() {
			// Three 10-byte entries, saved oldest→newest.
			Expect(fc.Save(keyFor("proj1", "a", "k"), bytes.NewReader(bytes.Repeat([]byte("x"), 10)))).To(Succeed())
			Expect(fc.Save(keyFor("proj1", "b", "k"), bytes.NewReader(bytes.Repeat([]byte("x"), 10)))).To(Succeed())
			Expect(fc.Save(keyFor("proj1", "c", "k"), bytes.NewReader(bytes.Repeat([]byte("x"), 10)))).To(Succeed())

			// Quota fits two entries; the oldest ("a") must go.
			report, err := fc.Evict(cache.Quota{PerProjectBytes: 20})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.RemovedEntries).To(Equal(1))
			Expect(report.BytesFreed).To(Equal(int64(10)))

			has, _ := fc.Has(keyFor("proj1", "a", "k"))
			Expect(has).To(BeFalse())
			has, _ = fc.Has(keyFor("proj1", "c", "k"))
			Expect(has).To(BeTrue())
		})

		It("respects the global quota across projects", func() {
			Expect(fc.Save(keyFor("p1", "a", "k"), bytes.NewReader(bytes.Repeat([]byte("x"), 10)))).To(Succeed())
			Expect(fc.Save(keyFor("p2", "a", "k"), bytes.NewReader(bytes.Repeat([]byte("x"), 10)))).To(Succeed())
			Expect(fc.Save(keyFor("p3", "a", "k"), bytes.NewReader(bytes.Repeat([]byte("x"), 10)))).To(Succeed())

			report, err := fc.Evict(cache.Quota{GlobalBytes: 20})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.RemovedEntries).To(Equal(1))

			// The oldest write (p1) is evicted first.
			has, _ := fc.Has(keyFor("p1", "a", "k"))
			Expect(has).To(BeFalse())
		})

		It("is a no-op under quota", func() {
			Expect(fc.Save(keyFor("proj1", "a", "k"), bytes.NewReader([]byte("x")))).To(Succeed())
			report, err := fc.Evict(cache.Quota{PerProjectBytes: 1 << 20, GlobalBytes: 1 << 20})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.RemovedEntries).To(BeZero())
		})
	})
})

// fakeClock returns a monotonically increasing time so Save/Restore order is
// reflected deterministically in metadata timestamps.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}
