package store_test

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aholstenson/kvarn/internal/nixcache/nixbase32"
	"github.com/aholstenson/kvarn/internal/nixcache/store"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// narNameOf returns the file name Nix would publish data under.
func narNameOf(data []byte) string {
	sum := sha256.Sum256(data)
	return nixbase32.Encode(sum[:]) + ".nar.xz"
}

const storeHash = "0mdqa9w1p6cmli6976v4wi0sw9r4p5pr"

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

	Describe("narinfo", func() {
		It("round-trips a record per upstream", func() {
			body := []byte("StorePath: /nix/store/" + storeHash + "-hello\n")
			Expect(s.WriteNarInfo("cache.nixos.org", storeHash, body)).To(Succeed())

			got, hit, err := s.ReadNarInfo("cache.nixos.org", storeHash)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(got).To(Equal(body))

			_, hit, err = s.ReadNarInfo("other.example", storeHash)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())
		})

		It("rejects a hash that is not a store path hash", func() {
			Expect(s.WriteNarInfo("cache.nixos.org", "../etc/passwd", []byte("x"))).NotTo(Succeed())
			_, _, err := s.ReadNarInfo("cache.nixos.org", "short")
			Expect(err).To(HaveOccurred())
		})

		It("rejects an upstream that is not a hostname", func() {
			Expect(s.WriteNarInfo("../up", storeHash, []byte("x"))).NotTo(Succeed())
		})
	})

	Describe("NAR files", func() {
		It("round-trips a file named by its hash", func() {
			data := []byte("nar-bytes")
			name := narNameOf(data)

			n, err := s.WriteNar(name, bytes.NewReader(data))
			Expect(err).NotTo(HaveOccurred())
			Expect(n).To(Equal(int64(len(data))))

			rc, size, hit, err := s.OpenNar(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			Expect(size).To(Equal(int64(len(data))))
			got, _ := io.ReadAll(rc)
			rc.Close()
			Expect(got).To(Equal(data))
		})

		It("refuses to commit bytes that do not hash to the name", func() {
			name := narNameOf([]byte("the real content"))
			_, err := s.WriteNar(name, bytes.NewReader([]byte("something else")))
			Expect(err).To(MatchError(store.ErrHashMismatch))

			_, _, hit, err := s.OpenNar(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())

			// No temp file is left behind either.
			entries, _ := filepath.Glob(filepath.Join(dir, "nar", "*", ".nar-*"))
			Expect(entries).To(BeEmpty())
		})

		It("reports a miss for an absent file", func() {
			_, _, hit, err := s.OpenNar(narNameOf([]byte("never written")))
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeFalse())
		})

		It("rejects names that are not hash.nar[.ext]", func() {
			_, err := s.WriteNar("../../escape.nar.xz", bytes.NewReader(nil))
			Expect(err).To(HaveOccurred())
			_, _, _, err = s.OpenNar("notahash.nar.xz")
			Expect(err).To(HaveOccurred())
		})

		It("survives concurrent writers of the same file", func() {
			data := bytes.Repeat([]byte("z"), 64*1024)
			name := narNameOf(data)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer GinkgoRecover()
					defer wg.Done()
					_, err := s.WriteNar(name, bytes.NewReader(data))
					Expect(err).NotTo(HaveOccurred())
				}()
			}
			wg.Wait()
			rc, _, hit, err := s.OpenNar(name)
			Expect(err).NotTo(HaveOccurred())
			Expect(hit).To(BeTrue())
			got, _ := io.ReadAll(rc)
			rc.Close()
			Expect(got).To(Equal(data))
		})
	})

	Describe("ParseNarName", func() {
		hash := nixbase32.Encode(bytes.Repeat([]byte{1}, 32))

		It("accepts the compressed and uncompressed forms", func() {
			for _, name := range []string{hash + ".nar", hash + ".nar.xz", hash + ".nar.zst", hash + ".nar.bz2"} {
				got, ok := store.ParseNarName(name)
				Expect(ok).To(BeTrue(), name)
				Expect(got).To(Equal(hash))
			}
		})

		It("rejects other shapes", func() {
			for _, name := range []string{hash, hash + ".narx", hash + ".nar.", hash + ".nar.X", hash + ".nar.xz/x", "x" + hash + ".nar"} {
				_, ok := store.ParseNarName(name)
				Expect(ok).To(BeFalse(), name)
			}
		})
	})

	Describe("stats and eviction", func() {
		It("counts files and bytes", func() {
			a := []byte("aaaa")
			b := []byte("bbbbbbbb")
			_, err := s.WriteNar(narNameOf(a), bytes.NewReader(a))
			Expect(err).NotTo(HaveOccurred())
			_, err = s.WriteNar(narNameOf(b), bytes.NewReader(b))
			Expect(err).NotTo(HaveOccurred())
			Expect(s.WriteNarInfo("cache.nixos.org", storeHash, []byte("x"))).To(Succeed())

			st, err := s.Stats()
			Expect(err).NotTo(HaveOccurred())
			Expect(st.NarCount).To(Equal(2))
			Expect(st.NarBytes).To(Equal(int64(12)))
			Expect(st.NarInfoCount).To(Equal(1))
		})

		It("evicts the least recently used NAR first and leaves narinfo alone", func() {
			old := []byte("old-old-old")
			fresh := []byte("fresh-fresh")
			_, err := s.WriteNar(narNameOf(old), bytes.NewReader(old))
			Expect(err).NotTo(HaveOccurred())
			now = now.Add(time.Hour)
			_, err = s.WriteNar(narNameOf(fresh), bytes.NewReader(fresh))
			Expect(err).NotTo(HaveOccurred())
			Expect(s.WriteNarInfo("cache.nixos.org", storeHash, []byte("x"))).To(Succeed())

			rep, err := s.EvictGlobal(int64(len(fresh)))
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.RemovedEntries).To(Equal(1))
			Expect(rep.BytesFreed).To(Equal(int64(len(old))))

			_, _, hit, _ := s.OpenNar(narNameOf(old))
			Expect(hit).To(BeFalse())
			_, _, hit, _ = s.OpenNar(narNameOf(fresh))
			Expect(hit).To(BeTrue())
			_, hit, _ = s.ReadNarInfo("cache.nixos.org", storeHash)
			Expect(hit).To(BeTrue())
		})

		It("treats a read as recent use", func() {
			first := []byte("first-first")
			second := []byte("second-secon")
			_, err := s.WriteNar(narNameOf(first), bytes.NewReader(first))
			Expect(err).NotTo(HaveOccurred())
			now = now.Add(time.Hour)
			_, err = s.WriteNar(narNameOf(second), bytes.NewReader(second))
			Expect(err).NotTo(HaveOccurred())
			now = now.Add(time.Hour)
			rc, _, _, err := s.OpenNar(narNameOf(first))
			Expect(err).NotTo(HaveOccurred())
			rc.Close()

			_, err = s.EvictGlobal(int64(len(first)))
			Expect(err).NotTo(HaveOccurred())
			_, _, hit, _ := s.OpenNar(narNameOf(first))
			Expect(hit).To(BeTrue())
			_, _, hit, _ = s.OpenNar(narNameOf(second))
			Expect(hit).To(BeFalse())
		})

		It("clears everything", func() {
			data := []byte("gone")
			_, err := s.WriteNar(narNameOf(data), bytes.NewReader(data))
			Expect(err).NotTo(HaveOccurred())
			Expect(s.WriteNarInfo("cache.nixos.org", storeHash, []byte("x"))).To(Succeed())
			Expect(s.Clear()).To(Succeed())
			_, err = os.Stat(filepath.Join(dir, "nar"))
			Expect(os.IsNotExist(err)).To(BeTrue())
			st, _ := s.Stats()
			Expect(st.NarCount).To(BeZero())
			Expect(st.NarInfoCount).To(BeZero())
		})
	})
})
