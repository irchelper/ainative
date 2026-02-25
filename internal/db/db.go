// Package db manages the SQLite connection and schema migrations.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS tasks (
  id                TEXT PRIMARY KEY,
  title             TEXT NOT NULL,
  description       TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT 'pending',
  assigned_to       TEXT NOT NULL DEFAULT '',
  retry_assigned_to TEXT NOT NULL DEFAULT '',
  superseded_by     TEXT NOT NULL DEFAULT '',
  parent_id         TEXT NOT NULL DEFAULT '',
  mode              TEXT NOT NULL DEFAULT '',
  requires_review   INTEGER NOT NULL DEFAULT 0,
  result            TEXT NOT NULL DEFAULT '',
  failure_reason    TEXT NOT NULL DEFAULT '',
  version           INTEGER NOT NULL DEFAULT 1,
  priority          INTEGER NOT NULL DEFAULT 0,
  started_at        DATETIME,
  created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS task_deps (
  task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  depends_on  TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  PRIMARY KEY (task_id, depends_on)
);

CREATE TABLE IF NOT EXISTS task_history (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  from_status TEXT NOT NULL DEFAULT '',
  to_status   TEXT NOT NULL,
  changed_by  TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  changed_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// Open initialises the SQLite database at path and runs schema migrations.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite works best with a single writer connection.
	db.SetMaxOpenConns(1)

	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("run schema: %w", err)
	}

	// Migrations: ALTER TABLE is silently ignored if the column already exists.
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN started_at DATETIME`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN failure_reason TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN retry_assigned_to TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN superseded_by TEXT NOT NULL DEFAULT ''`)

	return db, nil
}

// Ping verifies the database connection is alive.
func Ping(db *sql.DB) error {
	return db.Ping()
}
