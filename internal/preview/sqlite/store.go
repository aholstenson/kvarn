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
	apps := make([]preview.App, len(p.Apps))
	copy(apps, p.Apps)
	for i := range apps {
		apps[i].Host = preview.NormalizeHost(apps[i].Host)
	}
	appsJSON, err := json.Marshal(apps)
	if err != nil {
		return fmt.Errorf("encode preview apps: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin put preview: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO previews (
		    id, project, ref, state, apps_json, session_id, error,
		    created_at, updated_at, started_at, last_request_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    project = excluded.project,
		    ref = excluded.ref,
		    state = excluded.state,
		    apps_json = excluded.apps_json,
		    session_id = excluded.session_id,
		    error = excluded.error,
		    updated_at = excluded.updated_at,
		    started_at = excluded.started_at,
		    last_request_at = excluded.last_request_at,
		    expires_at = excluded.expires_at`,
		p.ID, p.Project, p.Ref, string(p.State), string(appsJSON), p.SessionID, p.Error,
		toMicros(p.CreatedAt), toMicros(p.UpdatedAt), toMicros(p.StartedAt),
		toMicros(p.LastRequestAt), toMicros(p.ExpiresAt),
	); err != nil {
		return fmt.Errorf("upsert preview: %w", err)
	}

	// Rewrite the hostname claims wholesale: the app set can change between
	// boots, and a name this preview no longer serves must not keep routing.
	if _, err := tx.ExecContext(ctx, `DELETE FROM preview_hosts WHERE preview_id = ?`, p.ID); err != nil {
		return fmt.Errorf("clear preview hosts: %w", err)
	}
	for _, app := range apps {
		if app.Host == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO preview_hosts (host, preview_id) VALUES (?, ?)`, app.Host, p.ID); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %s", preview.ErrHostTaken, app.Host)
			}
			return fmt.Errorf("claim preview host %q: %w", app.Host, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit put preview: %w", err)
	}
	return nil
}

const previewColumns = `id, project, ref, state, apps_json, session_id, error, ` +
	`created_at, updated_at, started_at, last_request_at, expires_at`

// scanPreview reads one row in previewColumns order.
func scanPreview(row interface{ Scan(...any) error }) (*preview.Preview, error) {
	var (
		p                                                         preview.Preview
		state, appsJSON                                           string
		createdAt, updatedAt, startedAt, lastRequestAt, expiresAt int64
	)
	if err := row.Scan(&p.ID, &p.Project, &p.Ref, &state, &appsJSON, &p.SessionID, &p.Error,
		&createdAt, &updatedAt, &startedAt, &lastRequestAt, &expiresAt); err != nil {
		return nil, err
	}
	p.State = preview.State(state)
	if err := json.Unmarshal([]byte(appsJSON), &p.Apps); err != nil {
		return nil, fmt.Errorf("decode preview apps for %q: %w", p.ID, err)
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

	if _, err := s.db.ExecContext(ctx, `
		UPDATE previews
		SET state = ?, started_at = 0, expires_at = 0, session_id = ''
		WHERE state != ?`, string(preview.StateStopped), string(preview.StateStopped)); err != nil {
		return nil, fmt.Errorf("reset live previews: %w", err)
	}
	return ids, nil
}
