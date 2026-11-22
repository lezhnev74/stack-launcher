package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrTemplateNameTaken indicates another template already has the requested name.
var ErrTemplateNameTaken = errors.New("template name already exists")

// Template holds a reusable, editable Compose manifest and its overrides.
type Template struct {
	Content     []byte
	Environment Environment
}

// AddManifestTemplate stores the uploaded bytes without replacing an existing template.
func (l *Ledger) AddManifestTemplate(ctx context.Context, name string, value Template) (bool, error) {
	env, err := json.Marshal(value.Environment)
	if err != nil {
		return false, err
	}
	result, err := l.db.ExecContext(ctx, `INSERT INTO manifest_templates (name, content, environment) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, name, value.Content, env)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// ManifestTemplates lists the names of the available templates.
func (l *Ledger) ManifestTemplates(ctx context.Context) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT name FROM manifest_templates ORDER BY name COLLATE NOCASE, name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// VisitTemplateManifests reads each template's source in name order.
func (l *Ledger) VisitTemplateManifests(ctx context.Context, visit func(name string, content []byte)) error {
	rows, err := l.db.QueryContext(ctx, `SELECT name, content FROM manifest_templates ORDER BY name COLLATE NOCASE, name`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var content []byte
		if err := rows.Scan(&name, &content); err != nil {
			return err
		}
		visit(name, content)
	}
	return rows.Err()
}

// ManifestTemplate reads a consistent snapshot of a template.
func (l *Ledger) ManifestTemplate(ctx context.Context, name string) (Template, error) {
	var value Template
	var env []byte
	if err := l.db.QueryRowContext(ctx, `SELECT content, environment FROM manifest_templates WHERE name = ?`, name).Scan(&value.Content, &env); err != nil {
		return value, err
	}
	var err error
	value.Environment, err = decodeEnvironment(env)
	return value, err
}

// UpdateManifestTemplate renames a template and replaces its configuration atomically.
func (l *Ledger) UpdateManifestTemplate(ctx context.Context, name, newName string, value Template) error {
	env, err := json.Marshal(value.Environment)
	if err != nil {
		return err
	}
	result, err := l.db.ExecContext(ctx, `UPDATE manifest_templates SET name = ?, content = ?, environment = ? WHERE name = ?`, newName, value.Content, env, name)
	if err != nil {
		return templateUpdateError(err)
	}
	if n, err := result.RowsAffected(); err != nil || n > 0 {
		return err
	}
	return sql.ErrNoRows
}

func templateUpdateError(err error) error {
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
		return ErrTemplateNameTaken
	}
	return err
}
