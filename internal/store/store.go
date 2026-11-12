// Package store opens AgentShiftboard's SQLite databases and applies the schema.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// SchemaVersion is bumped with every schema change that needs a migration.
const SchemaVersion = "1"

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	var v string
	err = db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		if _, err := db.Exec(`INSERT INTO meta(key,value) VALUES('schema_version',?)`, SchemaVersion); err != nil {
			db.Close()
			return nil, err
		}
	case err != nil:
		db.Close()
		return nil, err
	case v != SchemaVersion:
		db.Close()
		return nil, fmt.Errorf("database %s has schema version %s; this binary supports %s", path, v, SchemaVersion)
	}
	_ = os.Chmod(path, 0o600)
	return db, nil
}

// addedColumns lists columns added to a table after it first shipped.
// CREATE TABLE IF NOT EXISTS leaves an existing table as it was, so they
// are added here; each is additive with a default, so no version bump.
var addedColumns = []struct{ table, column, decl string }{
	{"messages", "claimed_at", "TEXT NOT NULL DEFAULT ''"},
}

func addColumns(db *sql.DB) error {
	for _, c := range addedColumns {
		// Check and add in one write transaction (BEGIN IMMEDIATE), so two
		// processes opening an old database do not both add the column.
		err := Tx(context.Background(), db, func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.column).Scan(&n); err != nil || n > 0 {
				return err
			}
			_, err := tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.decl))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Tx runs fn in a short write transaction.
func Tx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Now returns the current time as stored: RFC 3339 in UTC.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }

// OpenReadOnly opens an existing database without writing to it, for
// callers that cannot write there (for example inside an agent sandbox).
func OpenReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(5000)", path))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenMemory opens an empty in-memory database with the schema.
func OpenMemory() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
