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
  stale_dispatch_count     INTEGER NOT NULL DEFAULT 0,
  parent_id                TEXT NOT NULL DEFAULT '',
  mode                     TEXT NOT NULL DEFAULT '',
  requires_review          INTEGER NOT NULL DEFAULT 0,
  result                   TEXT NOT NULL DEFAULT '',
  failure_reason           TEXT NOT NULL DEFAULT '',
  version                  INTEGER NOT NULL DEFAULT 1,
  priority                 INTEGER NOT NULL DEFAULT 0,
  started_at               DATETIME,
  created_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  timeout_minutes          INTEGER,
  timeout_action           VARCHAR,
  commit_url               VARCHAR,
  auto_advance_to          TEXT NOT NULL DEFAULT '',
  advance_task_title       TEXT NOT NULL DEFAULT '',
  advance_task_description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS retry_routing (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  assigned_to       TEXT NOT NULL,
  error_keyword     TEXT NOT NULL DEFAULT '',
  retry_assigned_to TEXT NOT NULL,
  priority          INTEGER NOT NULL DEFAULT 0,
  UNIQUE(assigned_to, error_keyword)
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

CREATE TABLE IF NOT EXISTS templates (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL UNIQUE,
  description TEXT NOT NULL DEFAULT '',
  tasks_json  TEXT NOT NULL DEFAULT '[]',
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS task_comments (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  author     TEXT NOT NULL DEFAULT '',
  content    TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
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
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN stale_dispatch_count INTEGER NOT NULL DEFAULT 0`)
	// V12: AI Workbench schema extension (+3 fields).
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN timeout_minutes INTEGER`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN timeout_action VARCHAR`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN commit_url VARCHAR`)
	// V13: autoAdvance – success path dispatch (+3 fields).
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN auto_advance_to TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN advance_task_title TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN advance_task_description TEXT NOT NULL DEFAULT ''`)
	// V30-v4: spec_file — stores path to a local spec file for long task descriptions
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN spec_file TEXT NOT NULL DEFAULT ''`)
	// B8-B: ceo_notified_at — records when CEO was last notified about this failed task;
	// used by the 4h cleanup sweeper to auto-cancel acknowledged failed tasks.
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN ceo_notified_at DATETIME`)
	// ACTION-1 Phase1: acceptance — JSON-encoded list of acceptance criteria strings.
	_, _ = db.Exec(`ALTER TABLE tasks ADD COLUMN acceptance TEXT NOT NULL DEFAULT ''`)

	// Deduplicate retry_routing: keep the earliest row per (assigned_to, error_keyword) pair,
	// then add a UNIQUE index so INSERT OR IGNORE works correctly going forward.
	if err = deduplicateRetryRouting(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("deduplicate retry_routing: %w", err)
	}

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

	if err = seedTemplates(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed templates: %w", err)
	}

	return db, nil
}

// deduplicateRetryRouting removes duplicate rows in retry_routing (keeping the earliest per
// (assigned_to, error_keyword) pair) and creates a UNIQUE index so that subsequent
// INSERT OR IGNORE calls are actually idempotent.
func deduplicateRetryRouting(db *sql.DB) error {
	// Delete duplicates – keep the row with the lowest id for each unique key.
	_, err := db.Exec(`
		DELETE FROM retry_routing
		WHERE id NOT IN (
			SELECT MIN(id) FROM retry_routing GROUP BY assigned_to, error_keyword
		)
	`)
	if err != nil {
		return fmt.Errorf("deduplicate rows: %w", err)
	}

	// Create a unique index (silently ignored if it already exists).
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_retry_routing_key ON retry_routing(assigned_to, error_keyword)`)
	if err != nil {
		return fmt.Errorf("create unique index: %w", err)
	}

	return nil
}

// seedRetryRouting inserts the default 12-expert routing rules (idempotent via INSERT OR IGNORE).
func seedRetryRouting(db *sql.DB) error {
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
			`INSERT OR IGNORE INTO retry_routing (assigned_to, error_keyword, retry_assigned_to, priority) VALUES (?, ?, ?, ?)`,
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

	// V31-P0-3: coder spec 退单 → thinker（2026-02-28）
	// coder 退单原因含 "spec" 时直接路由 thinker 补充规格
	// V31-P0-4: coder agent_timeout → coder（自重试，不走 catch-all）
	v31Rules := []struct {
		assignedTo      string
		errorKeyword    string
		retryAssignedTo string
		priority        int
	}{
		{"coder", "spec", "thinker", 10},
		{"coder", "agent_timeout", "coder", 10},
	}
	for _, r := range v31Rules {
		_, err := db.Exec(
			`INSERT OR IGNORE INTO retry_routing (assigned_to, error_keyword, retry_assigned_to, priority) VALUES (?, ?, ?, ?)`,
			r.assignedTo, r.errorKeyword, r.retryAssignedTo, r.priority)
		if err != nil {
			return err
		}
	}

	// V10.1 视觉验收 + PM/Ops 补全（2026-02-26）
	v10Rules := []struct {
		assignedTo      string
		errorKeyword    string
		retryAssignedTo string
		priority        int
	}{
		{"vision", "设计", "uiux", 10},  // 视觉验收：设计问题退单 uiux
		{"vision", "", "coder", 0},      // 视觉验收：默认退单 coder
		{"pm", "", "thinker", 0},        // 需求问题升级架构
		{"ops", "", "devops", 0},        // 运维问题转 devops
	}
	for _, r := range v10Rules {
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

// seedTemplates inserts built-in template definitions (idempotent via INSERT OR IGNORE).
func seedTemplates(db *sql.DB) error {
	seeds := []struct {
		name  string
		desc  string
		tasks string
	}{
		{
			"fix-qa-deploy",
			"修复→QA验证→部署 标准三步链",
			`[{"assigned_to":"coder","title":"修复：{goal}","description":"实现修复"},{"assigned_to":"qa","title":"QA验证：{goal}","description":"验证修复结果"},{"assigned_to":"devops","title":"部署：{goal}","description":"发布到生产"}]`,
		},
		{
			"doc-review",
			"文档撰写→架构审核 两步链",
			`[{"assigned_to":"writer","title":"撰写文档：{goal}","description":"编写{goal}相关文档"},{"assigned_to":"thinker","title":"审核文档：{goal}","description":"审核文档质量和准确性"}]`,
		},
		{
			"feature",
			"需求→实现→审核→QA→部署 完整五步链",
			`[{"assigned_to":"pm","title":"需求分析：{goal}","description":"拆解{goal}需求"},{"assigned_to":"coder","title":"实现：{goal}","description":"按需求实现功能"},{"assigned_to":"thinker","title":"架构审核：{goal}","description":"审核实现方案"},{"assigned_to":"qa","title":"QA测试：{goal}","description":"测试功能"},{"assigned_to":"devops","title":"部署：{goal}","description":"发布上线"}]`,
		},
	}
	for _, s := range seeds {
		if _, err := db.Exec(
			`INSERT OR IGNORE INTO templates (name, description, tasks_json) VALUES (?, ?, ?)`,
			s.name, s.desc, s.tasks,
		); err != nil {
			return fmt.Errorf("seed template %q: %w", s.name, err)
		}
	}
	return nil
}
