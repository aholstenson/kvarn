// Package sqlite implements the session.Store interface on top of a pure-Go
// SQLite database (modernc.org/sqlite, CGo-free so it cross-compiles for the
// per-arch embedrunner builds). It provides durable session records plus a
// per-session monotonic event log.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/session"
	modernc "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	_ "modernc.org/sqlite"
)

// Store persists sessions and their event logs in a single SQLite database.
type Store struct {
	db *sql.DB
}

var _ session.Store = (*Store)(nil)

// New opens (creating if necessary) the sessions database at path and applies
// any pending migrations. The containing directory is created 0700 and the
// database file is chmod'd 0600 since it holds prompt/PR data.
func New(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create sessions dir: %w", err)
	}

	dsn := "file:" + path + "?" + strings.Join([]string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sessions db: %w", err)
	}
	// Low write volume; a single connection keeps writes serialized and avoids
	// SQLITE_BUSY between connections. Revisit with a small pool if poll/watch
	// reads contend.
	db.SetMaxOpenConns(1)

	// migrate runs the first query, which lazily creates the database file;
	// chmod afterwards so the path exists. Single-process access keeps the
	// brief default-perms window harmless.
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sessions db: %w", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chmod sessions db: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateSession(ctx context.Context, sess *session.Session) error {
	row, err := session.SessionToRow(sess)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sessions
		   (id, project_name, prompt, mode, state, message, error, pull_request_url, cost_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.ProjectName, row.Prompt, row.Mode, row.State, row.Message,
		row.Error, row.PullRequestURL, row.CostJSON, row.CreatedAt, row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

const sessionColumns = `id, project_name, prompt, mode, state, message, error, pull_request_url, cost_json, created_at, updated_at`

func scanSession(scan func(dest ...any) error) (*session.Session, error) {
	var r session.Row
	if err := scan(&r.ID, &r.ProjectName, &r.Prompt, &r.Mode, &r.State, &r.Message,
		&r.Error, &r.PullRequestURL, &r.CostJSON, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return session.RowToSession(r)
}

func (s *Store) GetSession(ctx context.Context, id string) (*session.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) UpdateSession(ctx context.Context, sess *session.Session) error {
	row, err := session.SessionToRow(sess)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		    SET state = ?, message = ?, error = ?, pull_request_url = ?, cost_json = ?, updated_at = ?
		  WHERE id = ?`,
		row.State, row.Message, row.Error, row.PullRequestURL, row.CostJSON, row.UpdatedAt, row.ID,
	)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("session %q not found", sess.ID)
	}
	return nil
}

func (s *Store) ListSessions(ctx context.Context, filter session.SessionFilter) ([]*session.Session, error) {
	var where []string
	var args []any
	if filter.Project != "" {
		where = append(where, "project_name = ?")
		args = append(args, filter.Project)
	}
	if filter.ActiveOnly {
		where = append(where, "state NOT IN (?, ?)")
		args = append(args, string(session.StateCompleted), string(session.StateFailed))
	}
	if filter.AfterID != "" {
		// Keyset cursor in (created_at DESC, id DESC) order.
		cursor := session.ToMicros(filter.AfterCreatedAt)
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor, cursor, filter.AfterID)
	}

	query := `SELECT ` + sessionColumns + ` FROM sessions`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []*session.Session
	for rows.Next() {
		sess, err := scanSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) AppendEvent(ctx context.Context, sessionID, kind string, payload []byte) (session.PersistedEvent, error) {
	recordedAt := time.Now().UTC()
	var (
		seq        int64
		recMicros  int64
	)
	err := retryBusy(func() error {
		// Seq assignment is index-backed by the (session_id, seq) PK; RETURNING
		// hands back the assigned values atomically.
		return s.db.QueryRowContext(ctx,
			`INSERT INTO session_events (session_id, seq, kind, payload, recorded_at)
			 SELECT ?, COALESCE((SELECT MAX(seq) FROM session_events WHERE session_id = ?), 0) + 1, ?, ?, ?
			 RETURNING seq, recorded_at`,
			sessionID, sessionID, kind, payload, session.ToMicros(recordedAt),
		).Scan(&seq, &recMicros)
	})
	if err != nil {
		return session.PersistedEvent{}, fmt.Errorf("append event: %w", err)
	}
	return session.PersistedEvent{
		SessionID:  sessionID,
		Seq:        seq,
		Kind:       kind,
		Payload:    payload,
		RecordedAt: session.FromMicros(recMicros),
	}, nil
}

func (s *Store) ListEvents(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]session.PersistedEvent, error) {
	query := `SELECT seq, kind, payload, recorded_at FROM session_events
	          WHERE session_id = ? AND seq > ? ORDER BY seq ASC`
	args := []any{sessionID, afterSeq}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []session.PersistedEvent
	for rows.Next() {
		var (
			ev        session.PersistedEvent
			recMicros int64
		)
		ev.SessionID = sessionID
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Payload, &recMicros); err != nil {
			return nil, err
		}
		ev.RecordedAt = session.FromMicros(recMicros)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) MaxSeq(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM session_events WHERE session_id = ?`, sessionID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("max seq: %w", err)
	}
	return seq, nil
}

func (s *Store) ReconcileNonTerminal(ctx context.Context, reason string) ([]string, error) {
	var ids []string
	err := retryBusy(func() error {
		ids = ids[:0]
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		rows, err := tx.QueryContext(ctx,
			`SELECT `+sessionColumns+` FROM sessions WHERE state NOT IN (?, ?) ORDER BY id`,
			string(session.StateCompleted), string(session.StateFailed))
		if err != nil {
			return err
		}
		var stale []*session.Session
		for rows.Next() {
			sess, err := scanSession(rows.Scan)
			if err != nil {
				rows.Close()
				return err
			}
			stale = append(stale, sess)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		now := time.Now().UTC()
		for _, sess := range stale {
			sess.State = session.StateFailed
			sess.Error = reason
			sess.UpdatedAt = now
			row, err := session.SessionToRow(sess)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET state = ?, error = ?, updated_at = ? WHERE id = ?`,
				row.State, row.Error, row.UpdatedAt, row.ID); err != nil {
				return err
			}
			kind, payload, err := session.EncodeStateChange(sess)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO session_events (session_id, seq, kind, payload, recorded_at)
				 SELECT ?, COALESCE((SELECT MAX(seq) FROM session_events WHERE session_id = ?), 0) + 1, ?, ?, ?`,
				sess.ID, sess.ID, kind, payload, session.ToMicros(now)); err != nil {
				return err
			}
			ids = append(ids, sess.ID)
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile non-terminal: %w", err)
	}
	return ids, nil
}

func (s *Store) PruneTerminalBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := retryBusy(func() error {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM sessions WHERE state IN (?, ?) AND created_at < ?`,
			string(session.StateCompleted), string(session.StateFailed), session.ToMicros(cutoff))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("prune terminal: %w", err)
	}
	return int(n), nil
}

// retryBusy retries fn while SQLite reports the database is busy/locked. With a
// single connection and busy_timeout this is rarely hit, but the loop guards
// against transient contention without surfacing it to callers.
func retryBusy(fn func() error) error {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		err = fn()
		if err == nil || !isBusy(err) {
			return err
		}
	}
	return err
}

func isBusy(err error) bool {
	var serr *modernc.Error
	if errors.As(err, &serr) {
		code := serr.Code()
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	return false
}
