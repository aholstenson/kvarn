package snapshot_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/preview/snapshot"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FileStore", func() {
	var (
		dir   string
		store *snapshot.FileStore
		now   time.Time
		id    snapshot.ID
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
		store = snapshot.NewFileStore(dir)
		store.Clock = func() time.Time { return now }
		id = snapshot.ID{ProjectID: "abc123", RefLabel: "feat-login"}
	})

	save := func(id snapshot.ID, body string, meta snapshot.Meta) {
		GinkgoHelper()
		Expect(store.Save(id, meta, strings.NewReader(body))).To(Succeed())
	}

	read := func(id snapshot.ID) (string, snapshot.Meta) {
		GinkgoHelper()
		r, meta, err := store.Open(id)
		Expect(err).NotTo(HaveOccurred())
		defer r.Close()
		data, err := io.ReadAll(r)
		Expect(err).NotTo(HaveOccurred())
		return string(data), meta
	}

	It("reports nothing stored before the first save", func() {
		_, _, err := store.Open(id)
		Expect(err).To(MatchError(snapshot.ErrNoSnapshot))

		_, err = store.Stat(id)
		Expect(err).To(MatchError(snapshot.ErrNoSnapshot))
	})

	It("round-trips an archive and its metadata", func() {
		save(id, "tarball-bytes", snapshot.Meta{Commit: "abc", Hosts: []string{"pr-1.example.test"}, Ref: "feat/login"})

		body, meta := read(id)
		Expect(body).To(Equal("tarball-bytes"))
		Expect(meta.Commit).To(Equal("abc"))
		Expect(meta.Hosts).To(ConsistOf("pr-1.example.test"))
		Expect(meta.Ref).To(Equal("feat/login"))
		Expect(meta.Bytes).To(Equal(int64(len("tarball-bytes"))))
		Expect(meta.CreatedAt).To(BeTemporally("==", now))
	})

	It("keeps refs apart within a project and projects apart from each other", func() {
		other := snapshot.ID{ProjectID: id.ProjectID, RefLabel: "main"}
		elsewhere := snapshot.ID{ProjectID: "def456", RefLabel: id.RefLabel}
		save(id, "one", snapshot.Meta{})
		save(other, "two", snapshot.Meta{})
		save(elsewhere, "three", snapshot.Meta{})

		body, _ := read(id)
		Expect(body).To(Equal("one"))
		body, _ = read(other)
		Expect(body).To(Equal("two"))
		body, _ = read(elsewhere)
		Expect(body).To(Equal("three"))
	})

	It("rotates the previous archive rather than overwriting it", func() {
		save(id, "first", snapshot.Meta{})
		save(id, "second", snapshot.Meta{})

		body, _ := read(id)
		Expect(body).To(Equal("second"))

		prev, err := os.ReadFile(filepath.Join(dir, id.ProjectID, id.RefLabel+".prev.tar.zst"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(prev)).To(Equal("first"))
	})

	It("leaves the stored archive alone when the source fails part-way", func() {
		save(id, "good", snapshot.Meta{})

		err := store.Save(id, snapshot.Meta{}, io.MultiReader(
			strings.NewReader("partial"), &failingReader{}))
		Expect(err).To(HaveOccurred())

		body, _ := read(id)
		Expect(body).To(Equal("good"))

		// The failed attempt must not have rotated the good copy away or left a
		// temp file behind for the next operator to wonder about.
		Expect(filepath.Join(dir, id.ProjectID, id.RefLabel+".prev.tar.zst")).NotTo(BeAnExistingFile())
		entries, err := os.ReadDir(filepath.Join(dir, id.ProjectID))
		Expect(err).NotTo(HaveOccurred())
		for _, e := range entries {
			Expect(e.Name()).NotTo(HavePrefix(".state-"))
		}
	})

	It("deletes both generations and the sidecar", func() {
		save(id, "first", snapshot.Meta{})
		save(id, "second", snapshot.Meta{})

		Expect(store.Delete(id)).To(Succeed())

		_, _, err := store.Open(id)
		Expect(err).To(MatchError(snapshot.ErrNoSnapshot))
		Expect(filepath.Join(dir, id.ProjectID, id.RefLabel+".prev.tar.zst")).NotTo(BeAnExistingFile())
		Expect(filepath.Join(dir, id.ProjectID, id.RefLabel+".meta")).NotTo(BeAnExistingFile())
	})

	It("treats deleting a preview with nothing stored as done", func() {
		Expect(store.Delete(id)).To(Succeed())
	})

	Describe("Prune", func() {
		BeforeEach(func() {
			save(id, "old", snapshot.Meta{})
		})

		It("removes an archive past the horizon", func() {
			now = now.Add(48 * time.Hour)

			report, err := store.Prune(24*time.Hour, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(Equal(1))
			Expect(report.BytesFreed).To(Equal(int64(len("old"))))

			_, _, err = store.Open(id)
			Expect(err).To(MatchError(snapshot.ErrNoSnapshot))
		})

		It("leaves an archive inside the horizon alone", func() {
			now = now.Add(time.Hour)

			report, err := store.Prune(24*time.Hour, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(BeZero())

			body, _ := read(id)
			Expect(body).To(Equal("old"))
		})

		It("moves the horizon when a restore touches the archive", func() {
			now = now.Add(20 * time.Hour)
			Expect(store.Touch(id)).To(Succeed())
			now = now.Add(20 * time.Hour)

			report, err := store.Prune(24*time.Hour, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(BeZero())
		})

		It("protects what keep names, so a live preview is never swept", func() {
			now = now.Add(48 * time.Hour)

			report, err := store.Prune(24*time.Hour, func(candidate snapshot.ID) bool {
				return candidate == id
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(BeZero())

			body, _ := read(id)
			Expect(body).To(Equal("old"))
		})

		It("never prunes when retention is zero", func() {
			now = now.Add(10000 * time.Hour)

			report, err := store.Prune(0, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(report.Removed).To(BeZero())

			body, _ := read(id)
			Expect(body).To(Equal("old"))
		})

		It("does not mistake a rotated generation for an archive of its own", func() {
			save(id, "new", snapshot.Meta{})

			ids, err := store.List()
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(ConsistOf(id))
		})
	})

	It("reports an archive with an unreadable sidecar from the file itself", func() {
		save(id, "bytes-here", snapshot.Meta{Commit: "abc"})
		Expect(os.WriteFile(filepath.Join(dir, id.ProjectID, id.RefLabel+".meta"),
			[]byte("not json"), 0o600)).To(Succeed())

		meta, err := store.Stat(id)
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.Bytes).To(Equal(int64(len("bytes-here"))))
	})

	It("refuses an ID that is not a single path element", func() {
		err := store.Save(snapshot.ID{ProjectID: "..", RefLabel: "x"}, snapshot.Meta{}, strings.NewReader("x"))
		Expect(err).To(HaveOccurred())

		err = store.Save(snapshot.ID{ProjectID: "p", RefLabel: "a/b"}, snapshot.Meta{}, strings.NewReader("x"))
		Expect(err).To(HaveOccurred())
	})
})

// failingReader fails on first read, standing in for a guest stream that dies
// half-way through a capture.
type failingReader struct{}

func (*failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
