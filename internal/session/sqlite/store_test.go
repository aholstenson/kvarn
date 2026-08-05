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

	It("applies the PR-fields migration over an existing database and keeps prior rows readable", func() {
		// Build a database at schema version 1 by hand — the state an
		// orchestrator that predates the PR fields left behind.
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		db, err := sql.Open("sqlite", path)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(`
			CREATE TABLE sessions (
			    id               TEXT PRIMARY KEY,
			    project_name     TEXT NOT NULL,
			    prompt           TEXT NOT NULL,
			    mode             TEXT NOT NULL,
			    state            TEXT NOT NULL,
			    message          TEXT NOT NULL DEFAULT '',
			    error            TEXT NOT NULL DEFAULT '',
			    pull_request_url TEXT NOT NULL DEFAULT '',
			    cost_json        TEXT NOT NULL DEFAULT '{}',
			    created_at       INTEGER NOT NULL,
			    updated_at       INTEGER NOT NULL
			);
			CREATE TABLE session_events (
			    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			    seq         INTEGER NOT NULL,
			    kind        TEXT NOT NULL,
			    payload     BLOB NOT NULL,
			    recorded_at INTEGER NOT NULL,
			    PRIMARY KEY (session_id, seq)
			);
			PRAGMA user_version = 1;`)
		Expect(err).NotTo(HaveOccurred())
		now := session.ToMicros(time.Now())
		_, err = db.Exec(
			`INSERT INTO sessions (id, project_name, prompt, mode, state, pull_request_url, created_at, updated_at)
			 VALUES ('old-1', 'proj', 'do it', 'auto', 'completed', 'https://example.com/pr/9', ?, ?)`,
			now, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(db.Close()).To(Succeed())

		store, err := sqlitestore.New(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(store.Close)

		got, err := store.GetSession(ctx, "old-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Prompt).To(Equal("do it"))
		Expect(got.PullRequestURL).To(Equal("https://example.com/pr/9"))
		// The new columns default to empty rather than NULL.
		Expect(got.PRRef).To(BeEmpty())
		Expect(got.HeadBranch).To(BeEmpty())
		Expect(got.BaseBranch).To(BeEmpty())
		Expect(got.ParentSessionID).To(BeEmpty())

		// The migrated row is writable through the new UPDATE.
		got.PRRef = "9"
		got.HeadBranch = "kvarn/thing"
		Expect(store.UpdateSession(ctx, got)).To(Succeed())
		reread, err := store.GetSession(ctx, "old-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(reread.PRRef).To(Equal("9"))

		byPR, err := store.ListSessions(ctx, session.SessionFilter{Project: "proj", PRRef: "9"})
		Expect(err).NotTo(HaveOccurred())
		Expect(byPR).To(HaveLen(1))
	})

	It("applies the mode-spec migration over a database that predates it", func() {
		// A database at the schema version before mode definitions and results
		// existed: every earlier migration applied, 0006 not yet.
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		db, err := sql.Open("sqlite", path)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Exec(`
			CREATE TABLE sessions (
			    id                TEXT PRIMARY KEY,
			    project_name      TEXT NOT NULL,
			    prompt            TEXT NOT NULL,
			    mode              TEXT NOT NULL,
			    state             TEXT NOT NULL,
			    message           TEXT NOT NULL DEFAULT '',
			    error             TEXT NOT NULL DEFAULT '',
			    pull_request_url  TEXT NOT NULL DEFAULT '',
			    pr_ref            TEXT NOT NULL DEFAULT '',
			    head_branch       TEXT NOT NULL DEFAULT '',
			    base_branch       TEXT NOT NULL DEFAULT '',
			    parent_session_id TEXT NOT NULL DEFAULT '',
			    cost_json         TEXT NOT NULL DEFAULT '{}',
			    created_at        INTEGER NOT NULL,
			    updated_at        INTEGER NOT NULL,
			    key_id            TEXT NOT NULL DEFAULT '',
			    priority          INTEGER NOT NULL DEFAULT 0,
			    attempts          INTEGER NOT NULL DEFAULT 0,
			    queued_at         INTEGER NOT NULL DEFAULT 0,
			    idempotency_key   TEXT NOT NULL DEFAULT '',
			    continuation      INTEGER NOT NULL DEFAULT 0
			);
			CREATE TABLE session_events (
			    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			    seq         INTEGER NOT NULL,
			    kind        TEXT NOT NULL,
			    payload     BLOB NOT NULL,
			    recorded_at INTEGER NOT NULL,
			    PRIMARY KEY (session_id, seq)
			);
			PRAGMA user_version = 5;`)
		Expect(err).NotTo(HaveOccurred())
		now := session.ToMicros(time.Now())
		_, err = db.Exec(
			`INSERT INTO sessions (id, project_name, prompt, mode, state, created_at, updated_at, queued_at)
			 VALUES ('old-1', 'proj', 'do it', 'review', 'completed', ?, ?, ?)`,
			now, now, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(db.Close()).To(Succeed())

		store, err := sqlitestore.New(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(store.Close)

		got, err := store.GetSession(ctx, "old-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Mode).To(Equal("review"))
		// The new columns default to empty rather than NULL, so a session that
		// predates them reads as one that named a mode and produced nothing.
		Expect(got.ModeSpecJSON).To(BeEmpty())
		Expect(got.Result).To(BeEmpty())

		got.Result = "Approve."
		Expect(store.UpdateSession(ctx, got)).To(Succeed())
		reread, err := store.GetSession(ctx, "old-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(reread.Result).To(Equal("Approve."))
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
