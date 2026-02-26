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
  id                       TEXT PRIMARY KEY,
  title                    TEXT NOT NULL,
  description              TEXT NOT NULL DEFAULT '',
  status                   TEXT NOT NULL DEFAULT 'pending',
  assigned_to              TEXT NOT NULL DEFAULT '',
  retry_assigned_to        TEXT NOT NULL DEFAULT '',
  superseded_by            TEXT NOT NULL DEFAULT '',
  chain_id                 TEXT NOT NULL DEFAULT '',
  notify_ceo_on_complete   INTEGER NOT NULL DEFAULT 0,
  parent_id                TEXT NOT NULL DEFAULT '',
  mode                     TEXT NOT NULL DEFAULT '',
  requires_review          INTEGER NOT NULL DEFAULT 0,
  result                   TEXT NOT NULL DEFAULT '',
  failure_reason           TEXT NOT NULL DEFAULT '',
  version                  INTEGER NOT NULL DEFAULT 1,
  priority                 INTEGER NOT NULL DEFAULT 0,
  started_at               DATETIME,
  created_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS retry_routing (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  assigned_to       TEXT NOT NULL,
  error_keyword     TEXT NOT NULL DEFAULT '',
  retry_assigned_to TEXT NOT NULL,
  priority          INTEGER NOT NULL DEFAULT 0
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
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN chain_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN notify_ceo_on_complete INTEGER NOT NULL DEFAULT 0`)

	// Seed retry_routing initial data (idempotent: only insert if table is empty).
	if err = seedRetryRouting(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed retry_routing: %w", err)
	}

	// Incremental retry_routing migrations (INSERT OR IGNORE — idempotent for existing instances).
	if err = addRetryRoutingRules(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("add retry_routing rules: %w", err)
	}

	return db, nil
}

// seedRetryRouting inserts the default 9-expert routing rules if the table is empty.
func seedRetryRouting(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM retry_routing`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil // already seeded
	}

	rules := []struct {
		assignedTo      string
		errorKeyword    string
		retryAssignedTo string
		priority        int
	}{
		{"qa", "bug", "coder", 10},
		{"qa", "ui", "uiux", 10},
		{"qa", "", "coder", 0},
		{"coder", "架构", "thinker", 10},
		{"coder", "需求", "pm", 10},
		{"coder", "", "thinker", 0},
		{"writer", "", "pm", 0},
		{"devops", "bug", "coder", 10},
		{"devops", "架构", "thinker", 10},
		{"devops", "", "coder", 0},
		{"thinker", "", "writer", 0},
		{"uiux", "", "pm", 0},
	}

	for _, r := range rules {
		_, err := db.Exec(
			`INSERT INTO retry_routing (assigned_to, error_keyword, retry_assigned_to, priority) VALUES (?, ?, ?, ?)`,
			r.assignedTo, r.errorKeyword, r.retryAssignedTo, r.priority)
		if err != nil {
			return err
		}
	}
	return nil
}

// addRetryRoutingRules inserts incremental routing rules using INSERT OR IGNORE (idempotent).
// Use this for rules added after the initial seed — safe to run on every startup.
func addRetryRoutingRules(db *sql.DB) error {
	// P2 审核链路退单规则（2026-02-26）
	// thinker REQUEST_CHANGES: 代码问题退 coder，文档问题退 writer
	// security REQUEST_CHANGES: 退 coder（兜底）
	p2Rules := []struct {
		assignedTo      string
		errorKeyword    string
		retryAssignedTo string
		priority        int
	}{
		{"thinker", "代码", "coder", 10},
		{"thinker", "文档", "writer", 10},
		{"security", "", "coder", 0},
	}
	for _, r := range p2Rules {
		_, err := db.Exec(
			`INSERT OR IGNORE INTO retry_routing (assigned_to, error_keyword, retry_assigned_to, priority) VALUES (?, ?, ?, ?)`,
			r.assignedTo, r.errorKeyword, r.retryAssignedTo, r.priority)
		if err != nil {
			return err
		}
	}
	return nil
}

// Ping verifies the database connection is alive.
func Ping(db *sql.DB) error {
	return db.Ping()
}
