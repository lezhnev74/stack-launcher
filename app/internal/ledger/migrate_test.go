package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestAdoptsUnversionedLedger(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE stacks (
		id TEXT PRIMARY KEY NOT NULL CHECK (id <> ''),
		hostname TEXT NOT NULL COLLATE NOCASE UNIQUE CHECK (hostname <> ''),
		duration_ns INTEGER NOT NULL CHECK (duration_ns > 0),
		state TEXT NOT NULL
	);
	INSERT INTO stacks VALUES ('old', 'Old.example', 123, 'ready');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got, err := l.Get(ctx, "old")
	if err != nil || got != (Stack{ID: "old", Hostname: "Old.example", Duration: 123 * time.Nanosecond, State: "ready"}) {
		t.Fatalf("legacy record: %+v, %v", got, err)
	}
	if err := l.Create(ctx, "new", "old.EXAMPLE", time.Second); err == nil {
		t.Fatal("accepted duplicate hostname")
	}
	files, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	assertVersion(t, l.db, len(files))
}

func assertVersion(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&got); err != nil || got != want {
		t.Fatalf("version = %d, %v; want %d", got, err, want)
	}
}

func TestMigrationRollbackAndRetry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	initial, err := fs.ReadFile(migrations, "migrations/001_create_stacks.sql")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fs.ReadFile(migrations, "migrations/002_manifest.sql")
	if err != nil {
		t.Fatal(err)
	}
	images, err := fs.ReadFile(migrations, "migrations/003_images.sql")
	if err != nil {
		t.Fatal(err)
	}
	api, err := fs.ReadFile(migrations, "migrations/004_api.sql")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := fs.ReadFile(migrations, "migrations/005_environment.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{
		"migrations/005_environment.sql":   {Data: environment},
		"migrations/001_create_stacks.sql": {Data: initial},
		"migrations/002_manifest.sql":      {Data: manifest},
		"migrations/003_images.sql":        {Data: images},
		"migrations/004_api.sql":           {Data: api},
	}
	dsn := filepath.Join(t.TempDir(), "stacks.sqlite3") + "?_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	l := &Ledger{db: db}
	defer func() { _ = l.Close() }()
	if err := runMigrations(ctx, db, source); err != nil {
		t.Fatal(err)
	}
	if err := l.Create(ctx, "one", "one.example", time.Second); err != nil {
		t.Fatal(err)
	}
	source["migrations/006_change.sql"] = &fstest.MapFile{Data: []byte(`
		ALTER TABLE stacks ADD COLUMN extra TEXT;
		UPDATE stacks SET state = 'changed';`)}
	source["migrations/007_next.sql"] = &fstest.MapFile{Data: []byte(`INSERT INTO missing_table VALUES (1);`)}
	err = runMigrations(ctx, db, source)
	if err == nil || !strings.Contains(err.Error(), "007_next.sql") {
		t.Fatalf("failure: %v", err)
	}
	assertVersion(t, db, 5)
	got, err := l.Get(ctx, "one")
	if err != nil || got.State != "launching" {
		t.Fatalf("rollback: %+v, %v", got, err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('stacks') WHERE name='extra'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("column survived rollback: %d, %v", count, err)
	}
	source["migrations/007_next.sql"] = &fstest.MapFile{Data: []byte(`UPDATE stacks SET extra = state;`)}
	if err := runMigrations(ctx, db, source); err != nil {
		t.Fatal(err)
	}
	assertVersion(t, db, 7)
	var extra string
	if err := db.QueryRowContext(ctx, "SELECT extra FROM stacks WHERE id='one'").Scan(&extra); err != nil || extra != "changed" {
		t.Fatalf("migration order: %q, %v", extra, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := runMigrations(ctx, reopened, source); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestMigrationValidation(t *testing.T) {
	t.Parallel()
	for _, names := range [][]string{
		{"wrong.sql"},
		{"002_missing.sql"},
		{"001_one.sql", "001_duplicate.sql"},
		{"000_zero.sql"},
		{"001_missing_extension"},
		{"001_.sql"},
		{"001_directory.sql/entry"},
	} {
		t.Run(fmt.Sprint(names), func(t *testing.T) {
			source := fstest.MapFS{}
			for _, name := range names {
				source["migrations/"+name] = &fstest.MapFile{}
			}
			if _, err := migrationFiles(source); err == nil {
				t.Fatal("accepted invalid filenames")
			}
		})
	}
	if _, err := migrationFiles(fstest.MapFS{}); err == nil {
		t.Fatal("accepted missing directory")
	}
}

func TestRejectUnsupportedVersion(t *testing.T) {
	t.Parallel()
	for _, version := range []int{-1, 1000000} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			l, err := Open(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			_, execErr := l.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version))
			closeErr := l.Close()
			if execErr != nil || closeErr != nil {
				t.Fatalf("set unsupported version %d: %v, %v", version, execErr, closeErr)
			}
			_, err = Open(ctx, dir)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("version %d", version)) ||
				!strings.Contains(err.Error(), "binary schema version") {
				t.Fatalf("unsupported version: %v", err)
			}
		})
	}
}

func TestVersionOneManifestUpgrade(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := fs.ReadFile(migrations, "migrations/001_create_stacks.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, string(initial)+` INSERT INTO stacks VALUES ('old', 'old.example', 123, 'launching'); PRAGMA user_version = 1;`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{Content: []byte{}, Repository: "team/repo", Commit: "abc", Path: "compose.yml", Checkout: "old"}
	if err = l.SaveManifest(ctx, "old", manifest); err != nil {
		t.Fatal(err)
	}
	if err = l.SaveManifest(ctx, "old", Manifest{Content: []byte("replacement")}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got, err := l.GetManifest(ctx, "old")
	if err != nil || got.Content == nil || len(got.Content) != 0 || got.Repository != manifest.Repository || got.Commit != manifest.Commit || got.Path != manifest.Path || got.Checkout != manifest.Checkout {
		t.Fatalf("manifest = %+v, %v", got, err)
	}
	stack, err := l.Get(ctx, "old")
	if err != nil || stack.Hostname != "old.example" || stack.Duration != 123 || stack.State != "launching" {
		t.Fatalf("stack = %+v, %v", stack, err)
	}
	if _, err = l.GetManifest(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
}

func TestV4EnvironmentMigration(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_create_stacks.sql", "002_manifest.sql", "003_images.sql", "004_api.sql"} {
		script, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(script)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO stacks (id, hostname, duration_ns, state) VALUES ('old', 'old.test', 1, 'ready'); PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	env, err := l.GetEnvironment(ctx, "old")
	if err != nil || len(env) != 0 {
		t.Fatalf("environment: %v %v", env, err)
	}
}
