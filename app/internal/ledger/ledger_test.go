package ledger

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceAndConstraints(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state #?%")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Stack{ID: "one", Hostname: "Example.com", Duration: 123456789 * time.Nanosecond, State: "launching"}
	if err := l.Create(ctx, want.ID, want.Hostname, want.Duration); err != nil {
		t.Fatal(err)
	}
	for _, record := range []struct {
		id, host string
		duration time.Duration
	}{
		{"two", "example.COM", time.Second},
		{"one", "other.example.com", time.Second},
		{"", "empty-id.example.com", time.Second},
		{"three", "", time.Second},
		{"four", "zero-duration.example.com", 0},
		{"five", "negative-duration.example.com", -time.Second},
	} {
		if err := l.Create(ctx, record.id, record.host, record.duration); err == nil {
			t.Errorf("accepted invalid record %+v", record)
		}
	}
	if _, err := l.Get(ctx, "two"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected ID: %v", err)
	}
	if err := l.Create(ctx, "two", "retry.example.com", time.Second); err != nil {
		t.Fatalf("retry with an available hostname: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got, err := l.Get(ctx, want.ID)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
}

func TestOpenFailures(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	if _, err := Open(ctx, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("opened missing directory")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "CREATE TABLE stacks (id TEXT, hostname TEXT, duration_ns INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("opened ledger missing the state column")
	}
	assertVersion(t, db, 0)
}

func TestReadOnlyDatabase(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	ctx := t.Context()
	dir := t.TempDir()
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stacks.sqlite3")
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir); err == nil {
		t.Fatal("opened read-only database")
	}
}

func TestRejectsLedgerSymlink(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "other.sqlite3")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(stateDir, "stacks.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), stateDir); err == nil {
		t.Fatal("opened a ledger outside the state directory")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatal("modified the ledger symlink target")
	}
}

func TestImagesPersistAndAreWriteOnce(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Create(ctx, "one", "one.example", time.Second); err != nil {
		t.Fatal(err)
	}
	images := map[string]Image{"web": {Reference: "example/web", ID: "abc"}}
	if err = l.SaveImages(ctx, "one", images); err == nil {
		t.Fatal("saved before manifest")
	}
	if err = l.SaveManifest(ctx, "one", Manifest{Content: []byte("source")}); err != nil {
		t.Fatal(err)
	}
	if err = l.SaveImages(ctx, "one", images); err != nil {
		t.Fatal(err)
	}
	if err = l.SaveImages(ctx, "one", images); err == nil {
		t.Fatal("replaced ownership")
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	got, err := l.GetImages(ctx, "one")
	if err != nil || got["web"] != images["web"] {
		t.Fatalf("%v %v", got, err)
	}
	if _, err = l.GetImages(ctx, "missing"); err == nil {
		t.Fatal("missing images")
	}
	if err = l.Create(ctx, "two", "two.example", time.Second); err != nil {
		t.Fatal(err)
	}
	if err = l.SaveManifest(ctx, "two", Manifest{}); err != nil {
		t.Fatal(err)
	}
	if _, err = l.db.ExecContext(ctx, `UPDATE stacks SET state = 'running' WHERE id = 'two'`); err != nil {
		t.Fatal(err)
	}
	if err = l.SaveImages(ctx, "two", images); err == nil {
		t.Fatal("saved after launch")
	}
}
