package ledger

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

//go:embed migrations
var migrations embed.FS

func migrationFiles(source fs.FS) ([]fs.DirEntry, error) {
	files, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, err
	}
	for i, file := range files {
		name := file.Name()
		prefix := fmt.Sprintf("%03d_", i+1)
		if file.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".sql") || name == prefix+".sql" {
			return nil, fmt.Errorf("invalid migration filename %q: expected %sdescription.sql", name, prefix)
		}
	}
	return files, nil
}

func runMigrations(ctx context.Context, db *sql.DB, source fs.FS) error {
	files, err := migrationFiles(source)
	if err != nil {
		return err
	}
	// The ledger DSN makes BeginTx acquire the write lock before reading the version.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := migrateSchema(ctx, tx, source, files); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateSchema(ctx context.Context, tx *sql.Tx, source fs.FS, files []fs.DirEntry) error {
	current, err := schemaVersion(ctx, tx, len(files))
	if err != nil {
		return err
	}
	if err := applyMigrations(ctx, tx, source, files[current:]); err != nil {
		return err
	}
	// Verify write access and required columns even when there is no pending migration.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO stacks (id, hostname, duration_ns, state, manifest, repository, commit_hash, manifest_path, checkout, images,
			api_owned, publication_pending, failure_phase, failure, url, bindings, ready_at, expires_at, environment)
		SELECT '', '', 0, '', NULL, NULL, NULL, NULL, NULL, NULL, 0, 0, '', '', '', NULL, '', '', NULL WHERE 0`); err != nil {
		return err
	}
	if current == len(files) {
		return nil
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(files)))
	return err
}

func schemaVersion(ctx context.Context, tx *sql.Tx, latest int) (int, error) {
	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return 0, err
	}
	if current < 0 || current > latest {
		return 0, fmt.Errorf("ledger schema version %d is incompatible with binary schema version %d", current, latest)
	}
	return current, nil
}

func applyMigrations(ctx context.Context, tx *sql.Tx, source fs.FS, files []fs.DirEntry) error {
	for _, file := range files {
		name := file.Name()
		script, err := fs.ReadFile(source, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}
