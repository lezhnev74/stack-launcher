package manifest

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

func referenceLedger(t *testing.T, ids ...string) (config.Config, *ledger.Ledger) {
	t.Helper()
	cfg := config.Config{StateDir: t.TempDir()}
	db, err := ledger.Open(t.Context(), cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, id := range ids {
		if err := db.Create(t.Context(), id, id+".example", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, db
}

func assertSnapshot(t *testing.T, saved ledger.Manifest, err error, content string) {
	t.Helper()
	if err != nil || string(saved.Content) != content || saved.Repository != "" || saved.Commit != "" || saved.Path != "" || saved.Checkout != "" {
		t.Fatalf("snapshot = %+v, %v", saved, err)
	}
}

func TestAcquireHTTPReference(t *testing.T) {
	ctx := t.Context()
	const content = "services:\r\n  app: {}"
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Referer() != "" {
			t.Error("redirect forwarded the referring URL to a different origin")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, content)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, target.URL+"/manifest", http.StatusTemporaryRedirect)
		case "/bad-redirect":
			w.Header().Set("Location", "%zz?"+r.URL.RawQuery)
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	cfg, db := referenceLedger(t, "web", "transport")
	const queryValue = "signed-query-sentinel"
	if _, err := Acquire(ctx, cfg, db, "web", Input{Reference: server.URL + "/bad-redirect?token=" + queryValue}); err == nil || strings.Contains(err.Error(), queryValue) {
		t.Fatalf("redirect failure should omit query: %v", err)
	}
	if _, err := Acquire(ctx, cfg, db, "web", Input{Reference: server.URL + "/loop"}); err == nil {
		t.Fatal("redirect loop was accepted")
	}
	if _, err := Acquire(ctx, cfg, db, "web", Input{Reference: server.URL + "/unavailable"}); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("unavailable manifest: %v", err)
	}
	if _, err := db.GetManifest(ctx, "web"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed fetch saved a manifest: %v", err)
	}
	got, err := Acquire(ctx, cfg, db, "web", Input{Reference: server.URL + "/redirect?token=" + queryValue})
	assertSnapshot(t, got, err, content)
	server.Close()
	saved, err := db.GetManifest(ctx, "web")
	assertSnapshot(t, saved, err, content)

	if _, err := Acquire(ctx, cfg, db, "transport", Input{Reference: server.URL + "/manifest?token=" + queryValue}); err == nil || strings.Contains(err.Error(), queryValue) {
		t.Fatalf("transport failure should omit query: %v", err)
	}
	if _, err := db.GetManifest(ctx, "transport"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed transport saved a manifest: %v", err)
	}
}

func TestReadHTTPSReference(t *testing.T) {
	const content = "services: {}\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()
	link, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readReferenceURL(t.Context(), link, server.Client())
	if err != nil || string(got) != content {
		t.Fatalf("HTTPS contents = %q, %v", got, err)
	}
}

func TestAcquireLocalReference(t *testing.T) {
	ctx := t.Context()
	cfg, db := referenceLedger(t, "absolute", "relative", "file-url")
	dir := t.TempDir()
	t.Chdir(dir)
	const name = "compose % fixture.yaml"
	const content = "services:\r\n  app: {}"
	path := filepath.Join(dir, name)
	write(t, path, content)
	fileURL := (&url.URL{Scheme: "file", Host: "localhost", Path: path}).String()
	references := map[string]string{"absolute": path, "relative": name, "file-url": fileURL}
	for id, reference := range references {
		got, err := Acquire(ctx, cfg, db, id, Input{Reference: reference})
		assertSnapshot(t, got, err, content)
	}
	write(t, path, "services: {}\n")
	for id := range references {
		saved, err := db.GetManifest(ctx, id)
		assertSnapshot(t, saved, err, content)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "checkouts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected checkout: %v", err)
	}
}

func TestRejectInvalidReference(t *testing.T) {
	ctx := t.Context()
	cfg, db := referenceLedger(t, "invalid")
	dir := t.TempDir()
	for _, test := range []struct{ reference, want string }{
		{"ftp://example.test/compose.yaml", "unsupported manifest scheme"},
		{"https://[broken", "invalid manifest reference URL"},
		{"file://remote.example/compose.yaml", "absolute path on the launcher host"},
		{"file:compose.yaml", "absolute path on the launcher host"},
		{dir, "must be a regular file"},
		{filepath.Join(dir, "missing"), "open local manifest"},
	} {
		if _, err := Acquire(ctx, cfg, db, "invalid", Input{Reference: test.reference}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("reference %q: %v, want %q", test.reference, err, test.want)
		}
	}
	if _, err := db.GetManifest(ctx, "invalid"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalid reference saved a manifest: %v", err)
	}
}

func TestReadReferenceContentErrors(t *testing.T) {
	if _, err := readReferenceContent(iotest.ErrReader(io.ErrUnexpectedEOF)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read failure: %v", err)
	}
	if _, err := readReferenceContent(strings.NewReader(strings.Repeat("a", maxReferenceSize+1))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized manifest: %v", err)
	}
}
