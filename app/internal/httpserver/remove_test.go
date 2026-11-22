package httpserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/ledger"
)

func createRemovalFixture(t *testing.T, a *api, id, host, checkout string, images bool) {
	t.Helper()
	ctx := t.Context()
	if err := a.db.CreateAPI(ctx, id, host, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SaveManifest(ctx, id, ledger.Manifest{Content: []byte("unusable manifest"), Checkout: checkout}); err != nil {
		t.Fatal(err)
	}
	if images {
		if err := a.db.SaveImages(ctx, id, map[string]ledger.Image{"web": {ID: strings.Repeat("a", 64), Reference: "example/web:v1"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func failRemovalFixture(t *testing.T, a *api, id string) {
	t.Helper()
	if changed, err := a.db.Fail(t.Context(), id, "startup", "fixture"); err != nil || !changed {
		t.Fatalf("record failure: changed=%t err=%v", changed, err)
	}
}

func makeRemovalCheckout(t *testing.T, a *api, name string) string {
	t.Helper()
	checkout := filepath.Join(a.cfg.StateDir, "checkouts", name)
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "keep.txt"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return checkout
}

func TestRemoveFencesRouteAndRetriesAfterRestart(t *testing.T) {
	a, r := setupAPI(t)
	checkout := makeRemovalCheckout(t, a, "checkout-123")
	createRemovalFixture(t, a, "one", "one.example.test", "checkout-123", true)
	ctx := t.Context()
	if err := a.db.Pending(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Ready(ctx, "one", "https://one.example.test", map[string]string{"web": "port"}, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	createRemovalFixture(t, a, "two", "two.example.test", "", true)
	r.abortErr = errors.New("Caddy unavailable")
	const path = "/stacks/one"
	calls := 0
	a.remove = func(_ context.Context, id string) error {
		calls++
		if id != "one" {
			t.Errorf("removed project %q", id)
		}
		if calls == 1 {
			return errors.New("volume busy")
		}
		return nil
	}
	if w := serve(a, http.MethodDelete, path, ""); w.Code != 502 || calls != 0 {
		t.Fatalf("withdraw failure: %d %s, Podman calls %d", w.Code, w.Body, calls)
	}
	s, err := a.db.Status(ctx, "one")
	if err != nil || s.State != "removing" || !s.Pending || s.URL != "" || len(s.Bindings) != 0 || s.ExpiresAt != "" {
		t.Fatalf("after withdraw failure: %+v %v", s, err)
	}
	r.abortErr = nil
	if w := serve(a, http.MethodDelete, path, ""); w.Code != 500 || calls != 1 {
		t.Fatalf("Podman failure: %d %s, Podman calls %d", w.Code, w.Body, calls)
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("checkout removed before Podman teardown completed: %v", err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a.db, err = ledger.Open(ctx, a.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.db.Close() })
	a.router = nil
	if err := a.recover(ctx); err != nil {
		t.Fatal(err)
	}
	s, err = a.db.Status(ctx, "one")
	if err != nil || s.State != "removing" || s.Pending {
		t.Fatalf("after restart: %+v %v", s, err)
	}
	if w := serve(a, http.MethodDelete, path, ""); w.Code != 204 || calls != 2 || r.aborted != 2 {
		t.Fatalf("retry without ingress: %d %s, Podman calls %d, Caddy calls %d", w.Code, w.Body, calls, r.aborted)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if w := serve(a, method, path, ""); w.Code != 404 {
			t.Errorf("%s removed stack: %d %s", method, w.Code, w.Body)
		}
	}
	if _, err := os.Stat(checkout); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("checkout remains: %v", err)
	}
	if w := serve(a, http.MethodGet, "/stacks/two", ""); w.Code != 200 {
		t.Errorf("other stack: %d %s", w.Code, w.Body)
	}
}

func TestRemoveEligibility(t *testing.T) {
	a, r := setupAPI(t)
	ctx := t.Context()
	if err := a.db.Create(ctx, "legacy", "legacy.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	createRemovalFixture(t, a, "launching", "launching.example.test", "", false)
	createRemovalFixture(t, a, "pending", "pending.different.test", "", false)
	if err := a.db.Pending(ctx, "pending"); err != nil {
		t.Fatal(err)
	}
	failRemovalFixture(t, a, "pending")
	saveReadyStack(t, a.db, "starting", "starting.example.test")
	for _, transition := range []func(context.Context, string) error{a.db.BeginStop, a.db.FinishStop, a.db.BeginStart} {
		if err := transition(ctx, "starting"); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]int{"unknown": 404, "legacy": 409, "launching": 409, "starting": 409, "pending": 503} {
		if w := serve(a, http.MethodDelete, "/stacks/"+id, ""); w.Code != want {
			t.Errorf("%s: %d %s", id, w.Code, w.Body)
		}
	}
	if s, err := a.db.Status(ctx, "pending"); err != nil || s.State != "failed" || !s.Pending || r.aborted != 0 {
		t.Fatalf("mismatched ingress changed stack: %+v %v, Caddy calls %d", s, err, r.aborted)
	}
	a.cfg.BaseDomain = "different.test"
	if w := serve(a, http.MethodDelete, "/stacks/pending", ""); w.Code != 204 || r.aborted != 1 {
		t.Errorf("failed pending stack: %d %s, Caddy calls %d", w.Code, w.Body, r.aborted)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, http.MethodDelete, "/stacks/legacy", ""); w.Code != 500 {
		t.Errorf("unavailable ledger: %d %s", w.Code, w.Body)
	}
}

func TestRemoveBeforePodmanProjectExists(t *testing.T) {
	a, _ := setupAPI(t)
	a.router = nil
	checkout := makeRemovalCheckout(t, a, "checkout-456")
	createRemovalFixture(t, a, "failed", "failed.example.test", "checkout-456", false)
	failRemovalFixture(t, a, "failed")
	if err := a.db.Create(t.Context(), "legacy", "legacy.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SaveManifest(t.Context(), "legacy", ledger.Manifest{Checkout: "checkout-456"}); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, http.MethodDelete, "/stacks/failed", ""); w.Code != 204 {
		t.Fatalf("remove failed stack: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(checkout, "keep.txt")); err != nil {
		t.Errorf("removed another stack's checkout: %v", err)
	}
	if err := a.db.CreateAPI(t.Context(), "acquisition", "acquisition.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	failRemovalFixture(t, a, "acquisition")
	if w := serve(a, http.MethodDelete, "/stacks/acquisition", ""); w.Code != 204 {
		t.Errorf("remove without saved manifest: %d %s", w.Code, w.Body)
	}
}

func TestRemoveRejectsSymlinkedCheckoutDirectory(t *testing.T) {
	a, _ := setupAPI(t)
	a.router = nil
	externalDir := t.TempDir()
	const name = "checkout-789"
	checkout := filepath.Join(externalDir, name)
	if err := os.Mkdir(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	checkouts := filepath.Join(a.cfg.StateDir, "checkouts")
	if err := os.Symlink(externalDir, checkouts); err != nil {
		t.Fatal(err)
	}
	createRemovalFixture(t, a, "one", "one.example.test", name, false)
	failRemovalFixture(t, a, "one")
	if w := serve(a, http.MethodDelete, "/stacks/one", ""); w.Code != 500 {
		t.Fatalf("remove via symlink: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(checkout); err != nil {
		t.Fatalf("external checkout: %v", err)
	}
	if err := os.Remove(checkouts); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, http.MethodDelete, "/stacks/one", ""); w.Code != 204 {
		t.Fatalf("retry with checkout already absent: %d %s", w.Code, w.Body)
	}
}

func TestRemoveSerializesWithOtherOperations(t *testing.T) {
	a, _ := setupAPI(t)
	createRemovalFixture(t, a, "one", "one.example.test", "", true)
	failRemovalFixture(t, a, "one")
	createRemovalFixture(t, a, "two", "two.example.test", "", false)
	failRemovalFixture(t, a, "two")
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan int, 1)
	a.remove = func(context.Context, string) error {
		close(started)
		<-release
		return nil
	}
	go func() { done <- serve(a, http.MethodDelete, "/stacks/one", "").Code }()
	defer func() {
		close(release)
		if code := <-done; code != 204 {
			t.Errorf("first remove: %d", code)
		}
	}()
	select {
	case <-started:
	case code := <-done:
		done <- code
		t.Fatalf("remove exited early: %d", code)
	}
	for _, tc := range []struct{ method, suffix, body string }{
		{http.MethodDelete, "", ""},
		{http.MethodPost, "/start", ""},
		{http.MethodPost, "/stop", ""},
		{http.MethodPut, "/environment", `{"environment":{}}`},
	} {
		if w := serve(a, tc.method, "/stacks/one"+tc.suffix, tc.body); w.Code != 409 {
			t.Errorf("overlapping %s %s: %d %s", tc.method, tc.suffix, w.Code, w.Body)
		}
	}
	if w := serve(a, http.MethodDelete, "/stacks/two", ""); w.Code != 204 {
		t.Errorf("second stack: %d %s", w.Code, w.Body)
	}
}
