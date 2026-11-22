package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Stack is a persisted stack record.
type Stack struct {
	ID       string        `json:"id"`
	Hostname string        `json:"hostname"`
	Duration time.Duration `json:"-"`
	State    string        `json:"state"`
}

// Ledger stores stack records in SQLite.
type Ledger struct{ db *sql.DB }

// Open opens the ledger in stateDir and applies pending schema migrations.
func Open(ctx context.Context, stateDir string) (*Ledger, error) {
	path, err := filepath.Abs(filepath.Join(stateDir, "stacks.sqlite3"))
	if err != nil {
		return nil, fmt.Errorf("ledger path %q: %w", stateDir, err)
	}
	if err := validateFile(path); err != nil {
		return nil, fmt.Errorf("ledger %q: %w", path, err)
	}
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?_txlock=immediate&_busy_timeout=5000&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("ledger %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := runMigrations(ctx, db, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ledger %q: initialize: %w", path, err)
	}
	return &Ledger{db: db}, nil
}

func validateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file")
	}
	return nil
}

// Create inserts a stack in its initial launching state.
func (l *Ledger) Create(ctx context.Context, id, hostname string, duration time.Duration) error {
	_, err := l.db.ExecContext(ctx, `INSERT INTO stacks (id, hostname, duration_ns, state) VALUES (?, ?, ?, ?)`, id, hostname, int64(duration), "launching")
	if err != nil {
		return fmt.Errorf("create stack %q: %w", id, err)
	}
	return nil
}

// Get returns a stack by ID.
func (l *Ledger) Get(ctx context.Context, id string) (Stack, error) {
	var stack Stack
	var duration int64
	err := l.db.QueryRowContext(ctx, `SELECT id, hostname, duration_ns, state FROM stacks WHERE id = ?`, id).Scan(&stack.ID, &stack.Hostname, &duration, &stack.State)
	if err != nil {
		return Stack{}, fmt.Errorf("get stack %q: %w", id, err)
	}
	stack.Duration = time.Duration(duration)
	return stack, nil
}

// Close releases the ledger database.
func (l *Ledger) Close() error { return l.db.Close() }

// Manifest records the exact Compose source and any Git provenance and checkout.
type Manifest struct {
	Content                            []byte
	Repository, Commit, Path, Checkout string
	References                         map[string]ReferenceOverride
}

// ReferenceOverride replaces the manifest's source ref for one service.
type ReferenceOverride struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// SaveManifest records the source once for a launching stack.
func (l *Ledger) SaveManifest(ctx context.Context, id string, manifest Manifest) error {
	content := manifest.Content
	if content == nil {
		content = []byte{}
	}
	result, err := l.db.ExecContext(ctx, `UPDATE stacks SET manifest = ?, repository = ?, commit_hash = ?, manifest_path = ?, checkout = ?,
		launch_request = json_remove(launch_request, '$.reference', '$.inline_yaml')
		WHERE id = ? AND state = 'launching' AND manifest IS NULL`, content, manifest.Repository, manifest.Commit, manifest.Path, manifest.Checkout, id)
	if err != nil {
		return fmt.Errorf("save manifest for stack %q: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save manifest for stack %q: %w", id, err)
	}
	if count != 1 {
		return fmt.Errorf("save manifest for stack %q: stack is missing, not launching, or already has a manifest", id)
	}
	return nil
}

// GetManifest returns the recorded Compose source.
func (l *Ledger) GetManifest(ctx context.Context, id string) (Manifest, error) {
	var manifest Manifest
	var refs []byte
	err := l.db.QueryRowContext(ctx, `SELECT manifest, repository, commit_hash, manifest_path, checkout, ref_overrides FROM stacks WHERE id = ? AND manifest IS NOT NULL`, id).Scan(&manifest.Content, &manifest.Repository, &manifest.Commit, &manifest.Path, &manifest.Checkout, &refs)
	if err != nil {
		return Manifest{}, fmt.Errorf("get manifest for stack %q: %w", id, err)
	}
	if manifest.Content == nil {
		manifest.Content = []byte{}
	}
	if len(refs) > 0 {
		err = json.Unmarshal(refs, &manifest.References)
	}
	return manifest, err
}

// Image records the immutable local image ID and the source used to select it.
// Git-backed images include the repository and resolved commit. Reference holds
// the registry image used for a pull and is empty for a local build.
type Image struct {
	Reference  string
	ID         string
	Repository string `json:",omitempty"`
	Commit     string `json:",omitempty"`
}

// SaveImages records the selected images once for a launching stack with a manifest.
func (l *Ledger) SaveImages(ctx context.Context, id string, images map[string]Image) error {
	data, err := json.Marshal(images)
	if err != nil {
		return fmt.Errorf("save images for stack %q: %w", id, err)
	}
	result, err := l.db.ExecContext(ctx, `UPDATE stacks SET images = ? WHERE id = ? AND state = 'launching' AND manifest IS NOT NULL AND images IS NULL`, data, id)
	if err != nil {
		return fmt.Errorf("save images for stack %q: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save images for stack %q: %w", id, err)
	}
	if count != 1 {
		return fmt.Errorf("save images for stack %q: stack is missing, not launching, has no manifest, or already has images", id)
	}
	return nil
}

// GetImages returns the recorded image ownership.
func (l *Ledger) GetImages(ctx context.Context, id string) (map[string]Image, error) {
	var data []byte
	if err := l.db.QueryRowContext(ctx, `SELECT images FROM stacks WHERE id = ? AND images IS NOT NULL`, id).Scan(&data); err != nil {
		return nil, fmt.Errorf("get images for stack %q: %w", id, err)
	}
	var images map[string]Image
	if err := json.Unmarshal(data, &images); err != nil {
		return nil, fmt.Errorf("get images for stack %q: %w", id, err)
	}
	return images, nil
}
