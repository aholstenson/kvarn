// Package sqlite implements the preview.Store interface on top of a pure-Go
// SQLite database (modernc.org/sqlite, CGo-free so it cross-compiles the same
// way the session store does). It sits beside internal/session/sqlite and
// follows the same shape: numbered migrations under migrations/, PRAGMA
// user_version, no migration library.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aholstenson/kvarn/internal/preview"
	modernc "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	_ "modernc.org/sqlite"
)

// Store persists previews and the hostnames that route to them.
type Store struct {
	db *sql.DB
}

var _ preview.Store = (*Store)(nil)

// DefaultPath returns the standard previews database location, mirroring the
// other stores under ~/.config/kvarn/.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "kvarn", "previews.db")
}

// New opens (creating if necessary) the previews database at path and applies
// any pending migrations.
func New(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create previews dir: %w", err)
	}

	dsn := "file:" + path + "?" + strings.Join([]string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open previews db: %w", err)
	}
	// Low write volume, and every write touches two tables in one transaction;
	// a single connection keeps them serialized and avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate previews db: %w", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chmod previews db: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// toMicros / fromMicros convert between time.Time and the unix-microsecond UTC
// integer persisted here, with zero mapping to 0 so an unset timestamp reads
// back as unset rather than as the epoch.
func toMicros(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMicro()
}

func fromMicros(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate key.
func isUniqueViolation(err error) bool {
	var serr *modernc.Error
	if !errors.As(err, &serr) {
		return false
	}
	code := serr.Code()
	return code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

func (s *Store) Put(ctx context.Context, p *preview.Preview) error {
	sites := make([]preview.Site, len(p.Sites))
	copy(sites, p.Sites)
	for i := range sites {
		sites[i].Host = preview.NormalizeHost(sites[i].Host)
	}
	sitesJSON, err := json.Marshal(sites)
	if err != nil {
		return fmt.Errorf("encode preview sites: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin put preview: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO previews (
		    id, project, ref, pr, auto_start_host, state, sites_json, session_id, error,
		    created_at, updated_at, started_at, last_request_at, expires_at,
		    state_saved_at, state_bytes, state_error, fork
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    project = excluded.project,
		    ref = excluded.ref,
		    pr = excluded.pr,
		    auto_start_host = excluded.auto_start_host,
		    state = excluded.state,
		    sites_json = excluded.sites_json,
		    session_id = excluded.session_id,
		    error = excluded.error,
		    updated_at = excluded.updated_at,
		    started_at = excluded.started_at,
		    last_request_at = excluded.last_request_at,
		    expires_at = excluded.expires_at,
		    state_saved_at = excluded.state_saved_at,
		    state_bytes = excluded.state_bytes,
		    state_error = excluded.state_error,
		    fork = excluded.fork`,
		p.ID, p.Project, p.Ref, p.PR, preview.NormalizeHost(p.AutoStartHost),
		string(p.State), string(sitesJSON), p.SessionID, p.Error,
		toMicros(p.CreatedAt), toMicros(p.UpdatedAt), toMicros(p.StartedAt),
		toMicros(p.LastRequestAt), toMicros(p.ExpiresAt),
		toMicros(p.StateSavedAt), p.StateBytes, p.StateError, p.Fork,
	); err != nil {
		return fmt.Errorf("upsert preview: %w", err)
	}

	// Rewrite the hostname claims wholesale: the site set can change between
	// boots, and a name this preview no longer serves must not keep routing.
	if _, err := tx.ExecContext(ctx, `DELETE FROM preview_hosts WHERE preview_id = ?`, p.ID); err != nil {
		return fmt.Errorf("clear preview hosts: %w", err)
	}
	for _, site := range sites {
		if site.Host == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO preview_hosts (host, preview_id) VALUES (?, ?)`, site.Host, p.ID); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", preview.ErrHostTaken, site.Host)
			}
			return fmt.Errorf("claim preview host %q: %w", site.Host, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit put preview: %w", err)
	}
	return nil
}

const previewColumns = `id, project, ref, pr, auto_start_host, state, sites_json, session_id, error, ` +
	`created_at, updated_at, started_at, last_request_at, expires_at, ` +
	`state_saved_at, state_bytes, state_error, fork`

// scanPreview reads one row in previewColumns order.
func scanPreview(row interface{ Scan(...any) error }) (*preview.Preview, error) {
	var (
		p                                                         preview.Preview
		state, sitesJSON                                          string
		createdAt, updatedAt, startedAt, lastRequestAt, expiresAt int64
		stateSavedAt                                              int64
	)
	if err := row.Scan(&p.ID, &p.Project, &p.Ref, &p.PR, &p.AutoStartHost,
		&state, &sitesJSON, &p.SessionID, &p.Error,
		&createdAt, &updatedAt, &startedAt, &lastRequestAt, &expiresAt,
		&stateSavedAt, &p.StateBytes, &p.StateError, &p.Fork); err != nil {
		return nil, err
	}
	p.StateSavedAt = fromMicros(stateSavedAt)
	p.State = preview.State(state)
	if err := json.Unmarshal([]byte(sitesJSON), &p.Sites); err != nil {
		return nil, fmt.Errorf("decode preview sites for %q: %w", p.ID, err)
	}
	p.CreatedAt = fromMicros(createdAt)
	p.UpdatedAt = fromMicros(updatedAt)
	p.StartedAt = fromMicros(startedAt)
	p.LastRequestAt = fromMicros(lastRequestAt)
	p.ExpiresAt = fromMicros(expiresAt)
	return &p, nil
}

func (s *Store) Get(ctx context.Context, id string) (*preview.Preview, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+previewColumns+` FROM previews WHERE id = ?`, id)
	p, err := scanPreview(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, preview.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get preview: %w", err)
	}
	return p, nil
}

func (s *Store) FindByHost(ctx context.Context, host string) (*preview.Preview, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+previewColumns+`
		FROM previews
		JOIN preview_hosts ON preview_hosts.preview_id = previews.id
		WHERE preview_hosts.host = ?`, preview.NormalizeHost(host))
	p, err := scanPreview(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, preview.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find preview by host: %w", err)
	}
	return p, nil
}

func (s *Store) List(ctx context.Context) ([]*preview.Preview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+previewColumns+` FROM previews ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list previews: %w", err)
	}
	defer rows.Close()

	var out []*preview.Preview
	for rows.Next() {
		p, err := scanPreview(rows)
		if err != nil {
			return nil, fmt.Errorf("scan preview: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Delete(ctx context.Context, id string) error {
	// preview_hosts cascades on the foreign key, so the claims go with the row.
	res, err := s.db.ExecContext(ctx, `DELETE FROM previews WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete preview: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete preview: %w", err)
	}
	if affected == 0 {
		return preview.ErrNotFound
	}
	return nil
}

func (s *Store) TouchRequest(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE previews SET last_request_at = ? WHERE id = ?`, toMicros(at), id); err != nil {
		return fmt.Errorf("touch preview: %w", err)
	}
	return nil
}

func (s *Store) ResetLive(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM previews WHERE state != ? ORDER BY id`,
		string(preview.StateStopped))
	if err != nil {
		return nil, fmt.Errorf("list live previews: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan live preview: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// state_error goes with the VM: a row left "stopping" by a crash had a
	// capture in flight that neither succeeded nor reported a reason, and a
	// stale message from the process before it would describe an attempt that is
	// no longer the last one. What the archive is — state_saved_at, state_bytes
	// — outlives the process and stays.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE previews
		SET state = ?, started_at = 0, expires_at = 0, session_id = '', state_error = ''
		WHERE state != ?`, string(preview.StateStopped), string(preview.StateStopped)); err != nil {
		return nil, fmt.Errorf("reset live previews: %w", err)
	}
	return ids, nil
}
