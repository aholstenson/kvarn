package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aholstenson/kvarn/internal/session"
	sqlitestore "github.com/aholstenson/kvarn/internal/session/sqlite"
)

var _ = Describe("sqlite.Store", func() {
	var (
		dir  string
		path string
		ctx  context.Context
	)

	BeforeEach(func() {
		dir = filepath.Join(GinkgoT().TempDir(), "nested")
		path = filepath.Join(dir, "sessions.db")
		ctx = context.Background()
	})

	It("creates the dir 0700 and the db file 0600", func() {
		store, err := sqlitestore.New(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(store.Close)

		di, err := os.Stat(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(di.Mode().Perm()).To(Equal(os.FileMode(0o700)))

		fi, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(fi.Mode().Perm()).To(Equal(os.FileMode(0o600)))
	})

	It("steps a fresh database to the latest migration version", func() {
		store, err := sqlitestore.New(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Close()).To(Succeed())

		db, err := sql.Open("sqlite", path)
		Expect(err).NotTo(HaveOccurred())
		defer db.Close()
		var version int
		Expect(db.QueryRow("PRAGMA user_version").Scan(&version)).To(Succeed())
		Expect(version).To(BeNumerically(">=", 1))
	})

	It("persists sessions and events across reopen (restart)", func() {
		store, err := sqlitestore.New(path)
		Expect(err).NotTo(HaveOccurred())

		s := &session.Session{
			ID:          "sess-1",
			ProjectName: "proj",
			Prompt:      "do it",
			Mode:        "auto",
			State:       session.StateRunning,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
		Expect(store.CreateSession(ctx, s)).To(Succeed())
		_, err = store.AppendEvent(ctx, "sess-1", "agent_message", []byte(`{"text":"hi"}`))
		Expect(err).NotTo(HaveOccurred())
		Expect(store.Close()).To(Succeed())

		reopened, err := sqlitestore.New(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(reopened.Close)

		got, err := reopened.GetSession(ctx, "sess-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ProjectName).To(Equal("proj"))

		max, err := reopened.MaxSeq(ctx, "sess-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(max).To(Equal(int64(1)))
	})
})
