package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/podman"
)

func stoppedStack(t *testing.T, a *api, id string) {
	t.Helper()
	saveReadyStack(t, a.db, id, id+".example.test")
	if err := a.db.BeginStop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.db.FinishStop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
}
func TestStartLifecycleAndContention(t *testing.T) {
	a, r := setupAPI(t)
	stoppedStack(t, a, "one")
	before := waitState(t, a, "one", "stopped")
	entered, unblock := make(chan struct{}), make(chan struct{})
	a.start = func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error) {
		close(entered)
		<-unblock
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:1234")}, nil
	}
	w := serve(a, http.MethodPost, "/stacks/one/start", "")
	if w.Code != 202 || w.Header().Get("Location") != "/stacks/one" {
		t.Fatalf("start: %d %v", w.Code, w.Header())
	}
	var accepted map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil || accepted["id"] != "one" {
		t.Fatalf("accepted response: %s (%v)", w.Body, err)
	}
	<-entered
	s := waitState(t, a, "one", "starting")
	if s.State != "starting" || s.ReadyAt != "" || s.ExpiresAt != "" {
		t.Fatalf("starting: %+v", s)
	}
	if w = serve(a, http.MethodPost, "/stacks/one/stop", ""); w.Code != 409 {
		t.Fatalf("stop: %d", w.Code)
	}
	if w = serve(a, http.MethodPost, "/stacks/one/start", ""); w.Code != 409 {
		t.Fatalf("start: %d", w.Code)
	}
	if w = serve(a, http.MethodPut, "/stacks/one/environment", `{"environment":{}}`); w.Code != 409 {
		t.Fatalf("edit environment: %d", w.Code)
	}
	close(unblock)
	s = waitState(t, a, "one", "ready")
	ready, _ := time.Parse(time.RFC3339Nano, s.ReadyAt)
	prior, _ := time.Parse(time.RFC3339Nano, before.ReadyAt)
	expiry, _ := time.Parse(time.RFC3339Nano, s.ExpiresAt)
	if s.Hostname != before.Hostname || s.URL != "http://one.example.test" || !ready.After(prior) || expiry.Sub(ready) != before.Duration {
		t.Fatalf("ready: %+v", s)
	}
	published, aborted := r.counts()
	if published != 1 || aborted != 1 {
		t.Fatalf("publish %d abort %d", published, aborted)
	}
}
func TestStartFailureAndRecovery(t *testing.T) {
	a, r := setupAPI(t)
	stoppedStack(t, a, "one")
	a.start = func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error) {
		return podman.Result{}, errors.New("broken")
	}
	if w := serve(a, http.MethodPost, "/stacks/one/start", ""); w.Code != 202 {
		t.Fatal(w.Code)
	}
	s := waitState(t, a, "one", "stopping")
	if s.FailurePhase != "startup" {
		t.Fatalf("failure: %+v", s)
	}
	a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error { return nil }
	stopAfterStartFailure(t, a, "/stacks/one/stop")
	if err := a.db.BeginStart(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Pending(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	r.abortErr = errors.New("Caddy unavailable")
	if err := a.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	s = waitState(t, a, "one", "stopping")
	if published, aborted := r.counts(); published != 0 || aborted != 1 || !s.Pending || s.FailurePhase != "interrupted" {
		t.Fatalf("recovery: %+v", s)
	}
	a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error {
		t.Error("removed project before withdrawal was confirmed")
		return nil
	}
	if w := serve(a, http.MethodPost, "/stacks/one/stop", ""); w.Code != 502 {
		t.Fatalf("stop with Caddy offline: %d", w.Code)
	}
	r.abortErr = nil
	a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error { return nil }
	if w := serve(a, http.MethodPost, "/stacks/one/stop", ""); w.Code != 204 {
		t.Fatalf("stop pending: %d", w.Code)
	}
	s, _ = a.db.Status(t.Context(), "one")
	if s.Pending {
		t.Fatal("pending marker retained")
	}
}

func stopAfterStartFailure(t *testing.T, a *api, path string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if w := serve(a, http.MethodPost, path, ""); w.Code != http.StatusConflict {
			if w.Code != http.StatusNoContent {
				t.Fatalf("stop: %d %s", w.Code, w.Body)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("operation still in progress for %s", path)
}

func TestStartRejectsUnavailableStacks(t *testing.T) {
	a, _ := setupAPI(t)
	stoppedStack(t, a, "one")
	saveReadyStack(t, a.db, "ready", "ready.example.test")
	check := func(id string, want int) {
		t.Helper()
		if w := serve(a, http.MethodPost, "/stacks/"+id+"/start", ""); w.Code != want {
			t.Fatalf("start %s: got %d, want %d: %s", id, w.Code, want, w.Body)
		}
	}
	check("missing", http.StatusNotFound)
	check("ready", http.StatusConflict)
	a.disabledReason = "different base domain"
	check("one", http.StatusServiceUnavailable)
	a.disabledReason = ""
	a.cfg.BaseDomain = "different.test"
	check("one", http.StatusServiceUnavailable)
	a.cfg.BaseDomain = "example.test"
	db, err := sql.Open("sqlite", filepath.Join(a.cfg.StateDir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_start BEFORE UPDATE OF state ON stacks WHEN NEW.state = 'starting' BEGIN SELECT RAISE(FAIL, 'cannot start'); END`); err != nil {
		t.Fatal(err)
	}
	check("one", http.StatusInternalServerError)
	waitState(t, a, "one", "stopped")
	_ = a.db.Close()
	check("one", http.StatusInternalServerError)
}

func TestStartFencesFailedPublication(t *testing.T) {
	a, r := setupAPI(t)
	stoppedStack(t, a, "one")
	a.start = func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error) {
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:1234")}, nil
	}
	r.publishErr = errors.New("unacknowledged publication")
	if w := serve(a, http.MethodPost, "/stacks/one/start", ""); w.Code != 202 {
		t.Fatal(w.Code)
	}
	s := waitState(t, a, "one", "stopping")
	if s.FailurePhase != "publication" || s.URL != "" {
		t.Fatalf("publication failure: %+v", s)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		s, _ = a.db.Status(t.Context(), "one")
		if !s.Pending {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if published, aborted := r.counts(); published != 1 || aborted != 2 || s.Pending {
		t.Fatalf("failed publication: %d publishes, %d aborts, pending %t", published, aborted, s.Pending)
	}
}

func TestStartRejectsFailedFenceBeforePublication(t *testing.T) {
	a, r := setupAPI(t)
	stoppedStack(t, a, "one")
	r.abortErr = errors.New("Caddy unavailable")
	a.start = func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error) {
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:1234")}, nil
	}
	if w := serve(a, http.MethodPost, "/stacks/one/start", ""); w.Code != 202 {
		t.Fatal(w.Code)
	}
	s := waitState(t, a, "one", "stopping")
	if published, aborted := r.counts(); s.FailurePhase != "publication" || s.Pending || published != 0 || aborted != 1 {
		t.Fatalf("failed fence: %+v, %d publishes, %d aborts", s, published, aborted)
	}
}
