package sqlite

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies every migration with an ordinal greater than the database's
// current PRAGMA user_version, each in its own transaction, then bumps the
// version. Migration files are named NNNN_name.sql; the leading ordinal is the
// version they advance the schema to. Lean by design — no migration library.
func migrate(db *sql.DB) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	type migration struct {
		version int
		name    string
	}
	var migrations []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var version int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &version); err != nil {
			return fmt.Errorf("migration %q: malformed name: %w", e.Name(), err)
		}
		migrations = append(migrations, migration{version: version, name: e.Name()})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })

	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for _, mig := range migrations {
		if mig.version <= current {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + mig.name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", mig.name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %q: %w", mig.name, err)
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %q: %w", mig.name, err)
		}
		// PRAGMA user_version doesn't accept bound parameters.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", mig.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("bump user_version for %q: %w", mig.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %q: %w", mig.name, err)
		}
	}
	return nil
}
