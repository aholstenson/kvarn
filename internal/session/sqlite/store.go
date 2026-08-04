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

// terminalStates renders the final states as a placeholder list plus its
// arguments, for the `state IN (...)` / `state NOT IN (...)` predicates. It is
// derived from session.TerminalStates so the queries follow that set.
func terminalStates() (string, []any) {
	states := session.TerminalStates()
	placeholders := make([]string, len(states))
	args := make([]any, len(states))
	for i, st := range states {
		placeholders[i] = "?"
		args[i] = string(st)
	}
	return strings.Join(placeholders, ", "), args
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
		   (id, project_name, prompt, mode, state, message, error, pull_request_url,
		    pr_ref, head_branch, base_branch, parent_session_id, cost_json, created_at, updated_at,
		    key_id, priority, attempts, queued_at, idempotency_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.ProjectName, row.Prompt, row.Mode, row.State, row.Message,
		row.Error, row.PullRequestURL, row.PRRef, row.HeadBranch, row.BaseBranch,
		row.ParentSessionID, row.CostJSON, row.CreatedAt, row.UpdatedAt,
		row.KeyID, row.Priority, row.Attempts, row.QueuedAt, row.IdempotencyKey,
	)
	if isUniqueViolation(err) {
		return session.ErrIdempotencyConflict
	}
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// FindByIdempotencyKey resolves the session that claimed key within project.
func (s *Store) FindByIdempotencyKey(ctx context.Context, project, key string) (*session.Session, error) {
	if key == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE project_name = ? AND idempotency_key = ?`,
		project, key)
	sess, err := scanSession(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sess, nil
}

const sessionColumns = `id, project_name, prompt, mode, state, message, error, pull_request_url, ` +
	`pr_ref, head_branch, base_branch, parent_session_id, cost_json, created_at, updated_at, ` +
	`key_id, priority, attempts, queued_at, idempotency_key`

func scanSession(scan func(dest ...any) error) (*session.Session, error) {
	var r session.Row
	if err := scan(&r.ID, &r.ProjectName, &r.Prompt, &r.Mode, &r.State, &r.Message,
		&r.Error, &r.PullRequestURL, &r.PRRef, &r.HeadBranch, &r.BaseBranch,
		&r.ParentSessionID, &r.CostJSON, &r.CreatedAt, &r.UpdatedAt,
		&r.KeyID, &r.Priority, &r.Attempts, &r.QueuedAt, &r.IdempotencyKey); err != nil {
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

// UpdateSession writes a run's mutable fields. The queue columns (key_id,
// priority, attempts, queued_at) are deliberately not among them: they are set
// once at submission and thereafter only by the backlog operations below, so an
// ordinary state update along the job's path cannot disturb the ordering or the
// attempt count.
func (s *Store) UpdateSession(ctx context.Context, sess *session.Session) error {
	row, err := session.SessionToRow(sess)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions
		    SET state = ?, message = ?, error = ?, pull_request_url = ?,
		        pr_ref = ?, head_branch = ?, base_branch = ?, cost_json = ?, updated_at = ?
		  WHERE id = ?`,
		row.State, row.Message, row.Error, row.PullRequestURL, row.PRRef,
		row.HeadBranch, row.BaseBranch, row.CostJSON, row.UpdatedAt, row.ID,
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
	if filter.PRRef != "" {
		where = append(where, "pr_ref = ?")
		args = append(args, filter.PRRef)
	}
	if filter.Mode != "" {
		where = append(where, "mode = ?")
		args = append(args, filter.Mode)
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, st := range filter.States {
			placeholders[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	if !filter.CreatedAfter.IsZero() {
		where = append(where, "created_at > ?")
		args = append(args, session.ToMicros(filter.CreatedAfter))
	}
	if filter.ActiveOnly {
		placeholders, stateArgs := terminalStates()
		where = append(where, "state NOT IN ("+placeholders+")")
		args = append(args, stateArgs...)
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
		seq       int64
		recMicros int64
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

func (s *Store) ListPending(ctx context.Context, q session.PendingQuery) ([]*session.Session, error) {
	// The ordering is computed rather than read off the index, so this sorts
	// the pending rows. The partial index is what keeps that set small: it is
	// the backlog, not the session history.
	pending := string(session.StatePending)
	query := `SELECT ` + sessionColumns + ` FROM sessions WHERE state = ? ORDER BY `
	args := []any{pending}
	if q.AgeStep > 0 {
		// Effective priority = configured + one level per AgeStep waited,
		// clamped to the highest priority in the backlog so aging can only
		// close a gap an operator opened. Mirrors scheduler.Fair.rank.
		query += `MIN(priority + (? - queued_at) / ?,
		              (SELECT MAX(priority) FROM sessions WHERE state = ?)) DESC, queued_at ASC`
		args = append(args, session.ToMicros(q.Now), q.AgeStep.Microseconds(), pending)
	} else {
		query += `priority DESC, queued_at ASC`
	}
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
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

func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE state = ?`, string(session.StatePending)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending: %w", err)
	}
	return n, nil
}

// UpdatePendingPriority rewrites the priority of a backlog entry. The state
// predicate is what makes it safe to run against a live dispatcher: an entry
// claimed between the caller's read and this write is left alone and reported
// as no longer pending, rather than having its ordering column rewritten after
// the ordering stopped mattering.
func (s *Store) UpdatePendingPriority(ctx context.Context, id string, priority int) (int, bool, error) {
	var (
		previous int
		updated  bool
	)
	err := retryBusy(func() error {
		previous, updated = 0, false
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// Read and write in one transaction so the priority reported as
		// replaced is the one this update actually replaced.
		err = tx.QueryRowContext(ctx,
			`SELECT priority FROM sessions WHERE id = ? AND state = ?`,
			id, string(session.StatePending)).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET priority = ?, updated_at = ? WHERE id = ?`,
			priority, session.ToMicros(time.Now()), id); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		updated = true
		return nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("update pending priority: %w", err)
	}
	return previous, updated, nil
}

func (s *Store) TransitionPending(ctx context.Context, id string, to session.PendingTransition) (bool, session.PersistedEvent, error) {
	var (
		claimed bool
		event   session.PersistedEvent
	)
	err := retryBusy(func() error {
		claimed = false
		event = session.PersistedEvent{}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		row := tx.QueryRowContext(ctx,
			`SELECT `+sessionColumns+` FROM sessions WHERE id = ? AND state = ?`,
			id, string(session.StatePending))
		sess, err := scanSession(row.Scan)
		if errors.Is(err, sql.ErrNoRows) {
			// Already claimed, cancelled or gone. Not an error: the caller
			// competing for it simply lost.
			return nil
		}
		if err != nil {
			return err
		}

		sess.State = to.State
		sess.Message = to.Message
		sess.Error = to.Error
		event, err = applyTransition(ctx, tx, sess, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	if err != nil {
		return false, session.PersistedEvent{}, fmt.Errorf("transition pending: %w", err)
	}
	return claimed, event, nil
}

func (s *Store) RequeueRun(ctx context.Context, id string, opts session.RequeueOpts) (bool, session.PersistedEvent, error) {
	var (
		requeued bool
		event    session.PersistedEvent
	)
	// The state predicate is built from session.RestartableStates so a state
	// added there is covered here without being named twice.
	restartable := session.RestartableStates()
	placeholders := make([]string, len(restartable))
	args := make([]any, 0, len(restartable)+1)
	args = append(args, id)
	for i, st := range restartable {
		placeholders[i] = "?"
		args = append(args, string(st))
	}
	query := `SELECT ` + sessionColumns + ` FROM sessions WHERE id = ? AND state IN (` +
		strings.Join(placeholders, ",") + `)`

	err := retryBusy(func() error {
		requeued = false
		event = session.PersistedEvent{}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		sess, err := scanSession(tx.QueryRowContext(ctx, query, args...).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			// The run advanced past the restartable states, or finished, while
			// it was being signalled. Not an error: the caller falls back to
			// recording the stop it asked for.
			return nil
		}
		if err != nil {
			return err
		}
		if opts.MaxAttempts > 0 && sess.Attempts >= opts.MaxAttempts {
			return nil
		}

		sess.Attempts++
		sess.State = session.StatePending
		sess.Message = opts.Message
		sess.Error = ""
		sess.QueuedAt = time.Now().UTC()
		event, err = applyTransition(ctx, tx, sess, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		requeued = true
		return nil
	})
	if err != nil {
		return false, session.PersistedEvent{}, fmt.Errorf("requeue run: %w", err)
	}
	return requeued, event, nil
}

func (s *Store) ExpirePending(ctx context.Context, cutoff time.Time, reason string) ([]string, error) {
	var ids []string
	err := retryBusy(func() error {
		ids = nil
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		stale, err := querySessions(ctx, tx,
			`SELECT `+sessionColumns+` FROM sessions WHERE state = ? AND queued_at < ? ORDER BY id`,
			string(session.StatePending), session.ToMicros(cutoff))
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		for _, sess := range stale {
			sess.State = session.StateFailed
			sess.Error = reason
			if _, err := applyTransition(ctx, tx, sess, now); err != nil {
				return err
			}
			ids = append(ids, sess.ID)
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, fmt.Errorf("expire pending: %w", err)
	}
	return ids, nil
}

func (s *Store) ReconcileStartup(ctx context.Context, opts session.ReconcileOpts) (session.ReconcileResult, error) {
	var result session.ReconcileResult
	err := retryBusy(func() error {
		result = session.ReconcileResult{}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// Pending sessions are excluded along with the terminal ones: they are
		// the backlog this reconciliation feeds, and touching them would charge
		// an attempt for a restart they sat out.
		placeholders, stateArgs := terminalStates()
		stateArgs = append(stateArgs, string(session.StatePending))
		stale, err := querySessions(ctx, tx,
			`SELECT `+sessionColumns+` FROM sessions WHERE state NOT IN (`+placeholders+`, ?) ORDER BY id`,
			stateArgs...)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		for _, sess := range stale {
			requeue := sess.State.IsRestartable()
			if requeue && opts.MaxAttempts > 0 && sess.Attempts >= opts.MaxAttempts {
				requeue = false
			}
			if requeue {
				sess.Attempts++
				sess.State = session.StatePending
				sess.Message = opts.RequeueMessage
				sess.Error = ""
				// QueuedAt moves so the entry ages from its return to the
				// backlog. Cost is not touched, so a job that already spent
				// against its cap carries that spend into the retry.
				sess.QueuedAt = now
				result.Requeued = append(result.Requeued, sess.ID)
			} else {
				sess.State = session.StateFailed
				sess.Error = opts.FailError
				if opts.MaxAttempts > 0 && sess.Attempts >= opts.MaxAttempts {
					sess.Error = fmt.Sprintf("%s (gave up after %d attempts)", opts.FailError, sess.Attempts)
				}
				result.Failed = append(result.Failed, sess.ID)
			}
			if _, err := applyTransition(ctx, tx, sess, now); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return session.ReconcileResult{}, fmt.Errorf("reconcile startup: %w", err)
	}
	return result, nil
}

// querySessions runs a session-shaped SELECT to completion inside tx. Reading
// the whole result before the next statement matters on a single-connection
// pool: an open rows cursor and a write on the same connection deadlock.
func querySessions(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]*session.Session, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
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

// applyTransition persists an already-mutated session and appends the
// state_change event recording it, in the caller's transaction so a watcher can
// never observe one without the other.
func applyTransition(ctx context.Context, tx *sql.Tx, sess *session.Session, now time.Time) (session.PersistedEvent, error) {
	sess.UpdatedAt = now
	row, err := session.SessionToRow(sess)
	if err != nil {
		return session.PersistedEvent{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions
		    SET state = ?, message = ?, error = ?, attempts = ?, queued_at = ?, updated_at = ?
		  WHERE id = ?`,
		row.State, row.Message, row.Error, row.Attempts, row.QueuedAt, row.UpdatedAt, row.ID); err != nil {
		return session.PersistedEvent{}, err
	}
	kind, payload, err := session.EncodeStateChange(sess)
	if err != nil {
		return session.PersistedEvent{}, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO session_events (session_id, seq, kind, payload, recorded_at)
		 SELECT ?, COALESCE((SELECT MAX(seq) FROM session_events WHERE session_id = ?), 0) + 1, ?, ?, ?
		 RETURNING seq`,
		sess.ID, sess.ID, kind, payload, session.ToMicros(now)).Scan(&seq); err != nil {
		return session.PersistedEvent{}, err
	}
	return session.PersistedEvent{
		SessionID:  sess.ID,
		Seq:        seq,
		Kind:       kind,
		Payload:    payload,
		RecordedAt: now,
	}, nil
}

func (s *Store) PruneTerminalBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	placeholders, args := terminalStates()
	args = append(args, session.ToMicros(cutoff))
	err := retryBusy(func() error {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM sessions WHERE state IN (`+placeholders+`) AND created_at < ?`,
			args...)
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

// isUniqueViolation reports whether err is a UNIQUE index violation. The only
// unique index an insert can trip is the idempotency one — the primary key is a
// generated id, and it reports its own extended code — so a caller reads this
// as "that key is already claimed".
func isUniqueViolation(err error) bool {
	var serr *modernc.Error
	if errors.As(err, &serr) {
		return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
	}
	return false
}

func isBusy(err error) bool {
	var serr *modernc.Error
	if errors.As(err, &serr) {
		code := serr.Code()
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	return false
}
