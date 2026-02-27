package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
)

// CreateTemplate inserts a new template. Returns ErrConflict if name already exists.
func (s *Store) CreateTemplate(req model.CreateTemplateRequest) (model.Template, error) {
	tasksJSON, err := json.Marshal(req.Tasks)
	if err != nil {
		return model.Template{}, fmt.Errorf("marshal tasks: %w", err)
	}
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO templates (name, description, tasks_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		req.Name, req.Description, string(tasksJSON), now, now,
	)
	if err != nil {
		// SQLite unique constraint violation
		return model.Template{}, fmt.Errorf("insert template: %w", err)
	}
	_ = res
	return s.GetTemplate(req.Name)
}

// GetTemplate returns a template by name.
func (s *Store) GetTemplate(name string) (model.Template, error) {
	row := s.db.QueryRow(
		`SELECT id, name, description, tasks_json, created_at, updated_at FROM templates WHERE name = ?`, name)
	return scanTemplate(row)
}

// GetTemplateByID returns a template by numeric id (for internal use).
func (s *Store) GetTemplateByID(id int64) (model.Template, error) {
	row := s.db.QueryRow(
		`SELECT id, name, description, tasks_json, created_at, updated_at FROM templates WHERE id = ?`, id)
	return scanTemplate(row)
}

// ListTemplates returns all templates ordered by name.
func (s *Store) ListTemplates() ([]model.Template, error) {
	rows, err := s.db.Query(
		`SELECT id, name, description, tasks_json, created_at, updated_at FROM templates ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
	var out []model.Template
	for rows.Next() {
		t, err := scanTemplateRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTemplate removes a template by name. Returns ErrNotFound if missing.
func (s *Store) DeleteTemplate(name string) error {
	res, err := s.db.Exec(`DELETE FROM templates WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// scanTemplate scans a *sql.Row into a Template.
func scanTemplate(row *sql.Row) (model.Template, error) {
	var t model.Template
	var tasksJSON string
	err := row.Scan(&t.ID, &t.Name, &t.Description, &tasksJSON, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return model.Template{}, ErrNotFound
	}
	if err != nil {
		return model.Template{}, fmt.Errorf("scan template: %w", err)
	}
	if err := json.Unmarshal([]byte(tasksJSON), &t.Tasks); err != nil {
		return model.Template{}, fmt.Errorf("parse tasks_json: %w", err)
	}
	return t, nil
}

// scanTemplateRow scans a *sql.Rows into a Template.
func scanTemplateRow(rows *sql.Rows) (model.Template, error) {
	var t model.Template
	var tasksJSON string
	err := rows.Scan(&t.ID, &t.Name, &t.Description, &tasksJSON, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return model.Template{}, fmt.Errorf("scan template row: %w", err)
	}
	if err := json.Unmarshal([]byte(tasksJSON), &t.Tasks); err != nil {
		return model.Template{}, fmt.Errorf("parse tasks_json: %w", err)
	}
	return t, nil
}
