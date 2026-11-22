package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

type stopRouter struct {
	*fakeRouter
	err   error
	calls int
}

func (r *stopRouter) Withdraw(context.Context, string) error { r.calls++; return r.err }

func saveReadyStack(t *testing.T, db *ledger.Ledger, id, host string) {
	t.Helper()
	ctx := t.Context()
	if err := db.CreateAPI(ctx, id, host, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.Pending(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := db.Ready(ctx, id, "https://"+host, map[string]string{"a": "b"}, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestStopRetry(t *testing.T) {
	a, base := setupAPI(t)
	r := &stopRouter{fakeRouter: base, err: errors.New("caddy")}
	a.router = r
	ctx := t.Context()
	saveReadyStack(t, a.db, "one", "one.example.test")
	calls := 0
	a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error {
		calls++
		if calls == 1 {
			return errors.New("podman")
		}
		return nil
	}
	request := func() int {
		return serve(a, http.MethodPost, "/stacks/one/stop", "").Code
	}
	if code := request(); code != 502 || calls != 0 {
		t.Fatalf("withdraw: %d calls %d", code, calls)
	}
	s, err := a.db.Status(ctx, "one")
	if err != nil || s.State != "stopping" || s.URL != "" || len(s.Bindings) != 0 || s.ExpiresAt != "" {
		t.Fatalf("status: %+v %v", s, err)
	}
	a.cfg.BaseDomain = "different.test"
	if err := a.recover(ctx); err != nil || a.disabledReason == "" {
		t.Fatalf("stopping stack must protect its original ingress: %q %v", a.disabledReason, err)
	}
	a.cfg.BaseDomain = "example.test"
	r.err = nil
	if code := request(); code != 500 {
		t.Fatalf("podman: %d", code)
	}
	if code := request(); code != 204 {
		t.Fatalf("retry: %d", code)
	}
	if w := serve(a, http.MethodGet, "/stacks/one", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"stopped"`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	}
	a.router = nil
	if code := request(); code != 204 || calls != 2 || r.calls != 3 {
		t.Fatalf("repeat: %d calls %d withdrawals %d", code, calls, r.calls)
	}
}

func TestStopRejectsUnownedAndUnavailableStacks(t *testing.T) {
	a, r := setupAPI(t)
	ctx := t.Context()
	for _, id := range []string{"launching", "failed"} {
		if err := a.db.CreateAPI(ctx, id, id+".example.test", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Fail(ctx, "failed", "startup", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Create(ctx, "legacy", "legacy.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(a.cfg.StateDir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `UPDATE stacks SET state = 'ready' WHERE id = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	saveReadyStack(t, a.db, "other", "other.different.test")
	for _, tc := range []struct {
		id, message string
		want        int
	}{
		{"unknown", "", 404},
		{"launching", "stack is launching", 409},
		{"failed", "state failed", 409},
		{"legacy", "created through the API", 409},
		{"other", "original Caddy instance", 503},
	} {
		w := serve(a, http.MethodPost, "/stacks/"+tc.id+"/stop", "")
		if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.message) {
			t.Errorf("%s: %d %s", tc.id, w.Code, w.Body)
		}
	}
	if w := serve(a, http.MethodGet, "/stacks/legacy", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"ready"`) {
		t.Fatalf("legacy GET: %d %s", w.Code, w.Body)
	}
	saveReadyStack(t, a.db, "ready", "ready.example.test")
	a.router = nil
	if w := serve(a, http.MethodPost, "/stacks/ready/stop", ""); w.Code != 503 || !strings.Contains(w.Body.String(), "caddy_admin_socket") {
		t.Fatalf("stop without ingress: %d %s", w.Code, w.Body)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a.router = r
	if w := serve(a, http.MethodPost, "/stacks/ready/stop", ""); w.Code != 500 {
		t.Fatalf("stop without ledger: %d %s", w.Code, w.Body)
	}
}

func TestStopRequestsSerializeByStack(t *testing.T) {
	a, _ := setupAPI(t)
	saveReadyStack(t, a.db, "one", "one.example.test")
	saveReadyStack(t, a.db, "two", "two.example.test")
	started, release := make(chan struct{}), make(chan struct{})
	a.stop = func(_ context.Context, _ config.Config, _ *ledger.Ledger, id string) error {
		if id == "one" {
			close(started)
			<-release
		}
		return nil
	}
	done := make(chan int, 1)
	go func() { done <- serve(a, http.MethodPost, "/stacks/one/stop", "").Code }()
	defer func() {
		// Let the request finish before the test closes the ledger.
		close(release)
		if code := <-done; code != 204 {
			t.Errorf("first stop: %d", code)
		}
	}()
	select {
	case <-started:
	case code := <-done:
		done <- code
		t.Fatalf("first stop exited early: %d", code)
	}
	if w := serve(a, http.MethodPost, "/stacks/one/stop", ""); w.Code != 409 {
		t.Errorf("overlapping stop: %d %s", w.Code, w.Body)
	}
	if w := serve(a, http.MethodPost, "/stacks/two/stop", ""); w.Code != 204 {
		t.Errorf("second stack: %d %s", w.Code, w.Body)
	}
}
