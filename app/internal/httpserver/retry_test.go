package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/podman"
)

func TestRetryFailedDraft(t *testing.T) {
	a, _ := setupAPI(t)
	const id, host = "draft", "draft.example.test"
	ctx := t.Context()
	manifest := ledger.Manifest{
		Content: []byte(runManifest), References: map[string]ledger.ReferenceOverride{"worker": {Kind: "git", Value: "release"}},
	}
	if err := a.db.CreateDraft(ctx, id, "original", time.Hour, ledger.Template{Content: manifest.Content}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SaveDraft(ctx, id, host, manifest, ledger.Environment{"web": {"MODE": "original"}}); err != nil {
		t.Fatal(err)
	}
	failForRetry(t, a, id)
	requirePage(t, a, "/ui/stacks?id="+id, `value="retry">Retry launch`, "fixture failure")
	if w := serve(a, http.MethodPut, "/stacks/"+id+"/environment", `{"environment":{"web":{"MODE":"fixed"}}}`); w.Code != 409 {
		t.Fatalf("replace overrides after launch: %d %s", w.Code, w.Body)
	}

	launch := a.launch
	entered, proceed := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	})
	a.launch = func(ctx context.Context, cfg config.Config, db *ledger.Ledger, id string, images map[string]string) (podman.Result, error) {
		saved, err := db.GetManifest(ctx, id)
		if err != nil || string(saved.Content) != runManifest || saved.References["worker"].Value != "release" {
			t.Errorf("retry manifest: %+v %v", saved, err)
		}
		env, err := db.GetEnvironment(ctx, id)
		if err != nil || env["web"]["MODE"] != "original" {
			t.Errorf("retry environment: %+v %v", env, err)
		}
		close(entered)
		<-proceed
		return launch(ctx, cfg, db, id, images)
	}
	checkRetry(t, a, id, 202)
	<-entered
	if s := waitState(t, a, id, "launching"); s.Failure != "" || s.Retryable || s.URL != "" {
		t.Fatalf("retry state: %+v", s)
	}
	checkRetry(t, a, id, 409)
	close(proceed)
	s := waitState(t, a, id, "ready")
	if s.Hostname != host || s.Duration != time.Hour || s.TemplateName != "original" || s.URL != "https://"+host {
		t.Fatalf("retry lost run identity or settings: %+v", s)
	}
}

func TestRetryAcquisitionThenReuseSavedInputs(t *testing.T) {
	a, _ := setupAPI(t)
	path := filepath.Join(a.cfg.StateDir, "manifest.yml")
	const image = "example/web:override"
	body, err := json.Marshal(map[string]any{"reference": path, "images": map[string]string{"web": image}})
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	launch := a.launch
	a.launch = func(ctx context.Context, cfg config.Config, db *ledger.Ledger, id string, images map[string]string) (podman.Result, error) {
		if images["web"] != image {
			t.Errorf("image overrides lost: %v", images)
		}
		if attempts.Add(1) == 1 {
			return podman.Result{}, errors.New("fixture image unavailable")
		}
		return launch(ctx, cfg, db, id, images)
	}
	id := postID(t, a, string(body))
	if s := waitState(t, a, id, "failed"); s.FailurePhase != "acquisition" || !s.Retryable || s.HasManifest {
		t.Fatalf("failed acquisition: %+v", s)
	}
	if err := os.WriteFile(path, []byte(runManifest), 0600); err != nil {
		t.Fatal(err)
	}
	waitRetryWorker(t, a, id)
	checkRetry(t, a, id, 202)
	if s := waitState(t, a, id, "failed"); s.FailurePhase != "startup" || !s.Retryable || !s.HasManifest {
		t.Fatalf("failure after acquisition: %+v", s)
	}
	saved, err := a.db.LaunchRequest(t.Context(), id)
	if err != nil || saved.Reference != "" || saved.Images["web"] != image {
		t.Fatalf("saved launch inputs: %+v %v", saved, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitRetryWorker(t, a, id)
	checkRetry(t, a, id, 202)
	waitState(t, a, id, "ready")
}

func TestRetryFencesPendingRouteBeforePinnedLaunch(t *testing.T) {
	a, r := setupAPI(t)
	ctx := t.Context()
	const id, host = "pinned", "pinned.example.test"
	if err := a.db.CreateAPI(ctx, id, host, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SaveManifest(ctx, id, ledger.Manifest{Content: []byte(runManifest)}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SaveImages(ctx, id, map[string]ledger.Image{"web": {ID: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Pending(ctx, id); err != nil {
		t.Fatal(err)
	}
	failForRetry(t, a, id)

	a.retryPinned = func(ctx context.Context, _ config.Config, db *ledger.Ledger, id string) (podman.Result, error) {
		s, err := db.Status(ctx, id)
		if err != nil || s.Pending || s.State != "launching" {
			t.Errorf("retry executed before withdrawal: %+v %v", s, err)
		}
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:1234")}, nil
	}
	r.abortErr = errors.New("Caddy unavailable")
	checkRetry(t, a, id, 502)
	if s := waitState(t, a, id, "failed"); !s.Pending {
		t.Fatalf("lost pending publication: %+v", s)
	}
	r.abortErr = nil
	checkRetry(t, a, id, 202)
	waitState(t, a, id, "ready")
	if published, aborted := r.counts(); published != 1 || aborted != 2 {
		t.Fatalf("published=%d aborted=%d", published, aborted)
	}
}

func TestRetryRejectsUnavailableRecords(t *testing.T) {
	a, _ := setupAPI(t)
	ctx := t.Context()
	checkRetry(t, a, "missing", 404)
	if err := a.db.CreateAPI(ctx, "old", "old.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	failForRetry(t, a, "old")
	checkRetry(t, a, "old", 409)

	request := &ledger.LaunchRequest{InlineYAML: []byte(runManifest)}
	if err := a.db.CreateLaunch(ctx, "new", "new.example.test", time.Hour, nil, "", request); err != nil {
		t.Fatal(err)
	}
	failForRetry(t, a, "new")
	a.cfg.BaseDomain = "different.test"
	checkRetry(t, a, "new", 503)
	a.cfg.BaseDomain = "example.test"
	db, err := sql.Open("sqlite", filepath.Join(a.cfg.StateDir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_retry BEFORE UPDATE OF state ON stacks
		WHEN NEW.state = 'launching' BEGIN SELECT RAISE(FAIL, 'cannot retry'); END`); err != nil {
		t.Fatal(err)
	}
	checkRetry(t, a, "new", 500)
	waitState(t, a, "new", "failed")
}

func failForRetry(t *testing.T, a *api, id string) {
	t.Helper()
	if changed, err := a.db.Fail(t.Context(), id, "startup", "fixture failure"); err != nil || !changed {
		t.Fatalf("failed fixture: %t %v", changed, err)
	}
}

func checkRetry(t *testing.T, a *api, id string, code int) {
	t.Helper()
	path := "/stacks/" + id
	w := serve(a, http.MethodPost, path+"/retry", "")
	if w.Code != code || (code == 202 && w.Header().Get("Location") != path) {
		t.Fatalf("retry: %d %s (%v)", w.Code, w.Body, w.Header())
	}
}

func waitRetryWorker(t *testing.T, a *api, id string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if _, running := a.operations.Load(id); !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operation still busy for %s", id)
}
