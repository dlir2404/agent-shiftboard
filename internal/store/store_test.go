package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// A database created before claimed_at existed gets the column on open.
func TestOpenAddsColumnsToExistingTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// Databases AgentShiftboard created are in WAL mode.
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id INTEGER NOT NULL,
	 from_session TEXT NOT NULL DEFAULT '', to_session TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
	 created_at TEXT NOT NULL, delivered_at TEXT NOT NULL DEFAULT '');
	 INSERT INTO messages(task_id,to_session,status,created_at) VALUES(1,'s','queued','t')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for i := 0; i < 2; i++ { // idempotent
		db, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var claimed string
		if err := db.QueryRow(`SELECT claimed_at FROM messages WHERE id=1`).Scan(&claimed); err != nil || claimed != "" {
			t.Fatalf("claimed_at=%q err=%v", claimed, err)
		}
		db.Close()
	}
}

// Several processes opening an old database at once all succeed.
func TestConcurrentUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	// Databases AgentShiftboard created are in WAL mode.
	old, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, from_session TEXT NOT NULL DEFAULT '',
	 to_session TEXT NOT NULL, body TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, created_at TEXT NOT NULL, delivered_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			db, err := Open(path)
			if err == nil {
				db.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < 6; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
