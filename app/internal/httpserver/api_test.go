package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/manifest"
	"stack-launcher.local/app/internal/podman"
)

type fakeRouter struct {
	mu                   sync.Mutex
	published, aborted   int
	publishErr, abortErr error
}

func (r *fakeRouter) Publish(_ context.Context, _ string, _ netip.AddrPort, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.published++
	return r.publishErr
}
func (r *fakeRouter) Abort(_ context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aborted++
	return r.abortErr
}
func (r *fakeRouter) Withdraw(context.Context, string) error { return nil }
func (r *fakeRouter) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.published, r.aborted
}
func setupAPI(t *testing.T) (*api, *fakeRouter) {
	t.Helper()
	dir := t.TempDir()
	db, err := ledger.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := &fakeRouter{}
	a := &api{ctx: t.Context(), cfg: config.Config{StateDir: dir, BaseDomain: "example.test", CaddyAdminSocket: "/tmp/caddy.sock"}, db: db, router: r, acquire: acquireManifest}
	a.launch = func(context.Context, config.Config, *ledger.Ledger, string, map[string]string) (podman.Result, error) {
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:1234"), HTTPS: true, Bindings: map[string]map[int]netip.AddrPort{"web": {80: netip.MustParseAddrPort("127.0.0.1:1234")}}}, nil
	}
	return a, r
}
func serve(a *api, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	a.handler().ServeHTTP(w, req)
	return w
}
func postID(t *testing.T, a *api, body string) string {
	t.Helper()
	w := serve(a, "POST", "/stacks", body)
	if w.Code != 202 {
		t.Fatalf("post %d: %s", w.Code, w.Body.String())
	}
	var data map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("Location") != "/stacks/"+data["id"] {
		t.Fatal("missing location")
	}
	return data["id"]
}

func TestLifecycleLogsUseRunID(t *testing.T) {
	a, _ := setupAPI(t)
	a.stop = func(context.Context, config.Config, *ledger.Ledger, string) error { return nil }
	a.start = func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error) {
		return podman.Result{}, errors.New("start failed\ncontainer exited")
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "launcher.log"))
	if err != nil {
		t.Fatal(err)
	}
	previous := log.Writer()
	log.SetOutput(file)
	t.Cleanup(func() {
		log.SetOutput(previous)
		_ = file.Close()
	})
	id := postID(t, a, `{"inline_yaml":"private manifest contents"}`)
	waitState(t, a, id, "ready")
	waitRetryWorker(t, a, id)
	path := "/stacks/" + id
	if w := serve(a, http.MethodPost, path+"/stop", ""); w.Code != 204 {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if w := serve(a, http.MethodPost, path+"/start", ""); w.Code != 202 {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	waitState(t, a, id, "stopping")
	waitRetryWorker(t, a, id)

	data, err := os.ReadFile(file.Name()) // #nosec G304 -- test-owned log in a temporary directory
	if err != nil {
		t.Fatal(err)
	}
	logs := string(data)
	if strings.Contains(logs, "private manifest contents") || strings.Contains(logs, "\ncontainer exited") {
		t.Fatalf("logs contain launch input or an unescaped error: %s", logs)
	}
	remaining := logs
	for _, message := range []string{
		"Launch accepted", "Acquiring manifest", "Manifest saved", "Publishing Caddy route", "Run ready",
		"Stop accepted", "Withdrawing Caddy route", "Stopping Podman project", "Run stopped", "Start accepted",
		`Run failed phase=startup publication_pending=false error="start failed\ncontainer exited"`, "Run failure recorded",
	} {
		_, after, found := strings.Cut(remaining, fmt.Sprintf("run_id=%q %s", id, message))
		if !found {
			t.Fatalf("missing lifecycle message %q for %s: %s", message, id, logs)
		}
		remaining = after
	}
}

func waitState(t *testing.T, a *api, id, state string) ledger.Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.db.Status(context.Background(), id)
		if err == nil && s.State == state {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stack %s did not reach %s", id, state)
	return ledger.Status{}
}
func TestAPIWorkerAndInputs(t *testing.T) {
	a, r := setupAPI(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var got manifest.Input
	a.acquire = func(ctx context.Context, cfg config.Config, db *ledger.Ledger, id string, in manifest.Input) (ledger.Manifest, error) {
		got = in
		if _, err := db.Get(ctx, id); err != nil {
			return ledger.Manifest{}, err
		}
		close(started)
		<-release
		return acquireManifest(ctx, cfg, db, id, in)
	}
	var override map[string]string
	a.launch = func(_ context.Context, _ config.Config, _ *ledger.Ledger, _ string, images map[string]string) (podman.Result, error) {
		override = images
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:99"), HTTPS: true}, nil
	}
	id := postID(t, a, `{"inline_yaml":"","duration":"2h","images":{"web":"example/web:v1"}}`)
	<-started
	if got.InlineYAML == nil || len(got.InlineYAML) != 0 {
		t.Fatal("explicit empty inline source lost")
	}
	if s, err := a.db.Status(t.Context(), id); err != nil || s.State != "launching" {
		t.Fatalf("record before work: %+v %v", s, err)
	}
	close(release)
	s := waitState(t, a, id, "ready")
	if s.URL != "https://"+id+".example.test" || override["web"] != "example/web:v1" {
		t.Fatalf("ready: %+v overrides %v", s, override)
	}
	ready, _ := time.Parse(time.RFC3339Nano, s.ReadyAt)
	expiry, _ := time.Parse(time.RFC3339Nano, s.ExpiresAt)
	if expiry.Sub(ready) != 2*time.Hour {
		t.Fatal("wrong expiry")
	}
	if w := serve(a, "GET", "/stacks/"+id, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"ready"`) {
		t.Fatalf("status: %s", w.Body.String())
	}
	if p, _ := r.counts(); p != 1 {
		t.Fatal("publication missing")
	}
}
func TestValidationAndFailures(t *testing.T) {
	a, _ := setupAPI(t)
	body := `{"inline_yaml":"x"}`
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/stacks", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	a.post(w, req)
	if w.Code != 415 {
		t.Fatalf("browser-safelisted content type: %d", w.Code)
	}
	for _, body := range []string{`{}`, `{"reference":""}`, `{"reference":"x","inline_yaml":""}`, `{"inline_yaml":"x","duration":"0s"}`, `{"inline_yaml":"x","foo":1}`, `{"inline_yaml":"x"} {}`, `{"inline_yaml":"x","images":{"web":"bad image"}}`} {
		if w := serve(a, "POST", "/stacks", body); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	if w := serve(a, "POST", "/stacks", `{"inline_yaml":"`+strings.Repeat("x", maxBody)+`"}`); w.Code != 413 {
		t.Fatalf("oversize %d", w.Code)
	}
	if w := serve(a, "GET", "/stacks/missing", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestReferenceAndFailures(t *testing.T) {
	a, r := setupAPI(t)
	path := filepath.Join(a.cfg.StateDir, "fixture")
	if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	a.launch = func(context.Context, config.Config, *ledger.Ledger, string, map[string]string) (podman.Result, error) {
		return podman.Result{}, errors.New("startup error")
	}
	id := postID(t, a, fmt.Sprintf(`{"reference":%q}`, path))
	s := waitState(t, a, id, "failed")
	if s.FailurePhase != "startup" || !strings.Contains(s.Failure, "startup error") {
		t.Fatal(s)
	}
	w := serve(a, "GET", "/stacks/"+id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"failure_phase":"startup"`) || !strings.Contains(w.Body.String(), `"failure":"startup error"`) {
		t.Fatalf("startup failure response: %d %s", w.Code, w.Body.String())
	}
	if m, err := a.db.GetManifest(t.Context(), id); err != nil || string(m.Content) != "source" {
		t.Fatalf("manifest %q %v", m.Content, err)
	}
	a.launch = func(context.Context, config.Config, *ledger.Ledger, string, map[string]string) (podman.Result, error) {
		return podman.Result{Endpoint: netip.MustParseAddrPort("127.0.0.1:1")}, nil
	}
	r.mu.Lock()
	r.publishErr = errors.New("lost acknowledgement")
	r.mu.Unlock()
	id = postID(t, a, `{"inline_yaml":"x"}`)
	s = waitState(t, a, id, "failed")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s, _ = a.db.Status(t.Context(), id)
		if !s.Pending {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s.Pending || s.FailurePhase != "publication" {
		t.Fatal(s)
	}
	w = serve(a, "GET", "/stacks/"+id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"failure_phase":"publication"`) || !strings.Contains(w.Body.String(), `"failure":"lost acknowledgement"`) {
		t.Fatalf("publication failure response: %d %s", w.Code, w.Body.String())
	}
	if _, n := r.counts(); n != 1 {
		t.Fatal("missing abort")
	}
}
func TestDatabaseFailures(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		publishes     int
	}{
		{"pending", `CREATE TRIGGER fail_update BEFORE UPDATE OF publication_pending ON stacks WHEN NEW.publication_pending=1 BEGIN SELECT RAISE(FAIL,'pending failure'); END`, 0},
		{"ready", `CREATE TRIGGER fail_update BEFORE UPDATE OF state ON stacks WHEN NEW.state='ready' BEGIN SELECT RAISE(FAIL,'ready failure'); END`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, r := setupAPI(t)
			db, err := sql.Open("sqlite", filepath.Join(a.cfg.StateDir, "stacks.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err = db.ExecContext(t.Context(), tc.trigger); err != nil {
				t.Fatal(err)
			}
			id := postID(t, a, `{"inline_yaml":"x"}`)
			waitState(t, a, id, "failed")
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				s, _ := a.db.Status(t.Context(), id)
				if !s.Pending {
					break
				}
				time.Sleep(time.Millisecond)
			}
			p, n := r.counts()
			if p != tc.publishes || n != tc.publishes {
				t.Fatalf("publish=%d abort=%d", p, n)
			}
		})
	}
}

func TestRecoveryPreservesOwnedResources(t *testing.T) {
	a, r := setupAPI(t)
	ctx := context.Background()
	for _, id := range []string{"interrupted", "ready", "legacy"} {
		if id == "legacy" {
			if err := a.db.Create(ctx, id, id+".example.test", time.Hour); err != nil {
				t.Fatal(err)
			}
		} else if err := a.db.CreateAPI(ctx, id, id+".example.test", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.db.SaveManifest(ctx, "legacy", ledger.Manifest{Content: []byte("x"), Checkout: "checkout-owned"}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Pending(ctx, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Pending(ctx, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Ready(ctx, "ready", "https://ready.example.test", nil, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"checkout-owned", "checkout-12345", "checkout-backup"} {
		if err := os.MkdirAll(filepath.Join(a.cfg.StateDir, "checkouts", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".compose-12345", ".compose-backup"} {
		if err := os.Mkdir(filepath.Join(a.cfg.StateDir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	r.abortErr = errors.New("offline")
	if err := a.recover(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ := a.db.Status(ctx, "interrupted")
	if s.State != "failed" || !s.Pending {
		t.Fatal(s)
	}
	for _, name := range []string{"checkouts/checkout-owned", "checkouts/checkout-backup", ".compose-backup"} {
		if _, err := os.Stat(filepath.Join(a.cfg.StateDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"checkouts/checkout-12345", ".compose-12345"} {
		if _, err := os.Stat(filepath.Join(a.cfg.StateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s remains", name)
		}
	}
	for _, pair := range [][2]string{{"legacy", "launching"}, {"ready", "ready"}} {
		s, _ := a.db.Status(ctx, pair[0])
		if s.State != pair[1] {
			t.Fatal(s)
		}
	}
	r.abortErr = nil
	a.withdrawFailed(ctx)
	s, _ = a.db.Status(ctx, "interrupted")
	if s.Pending {
		t.Fatal("pending not cleared")
	}
	a.cfg.BaseDomain = "different.test"
	if err := a.recover(ctx); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, "POST", "/stacks", `{"inline_yaml":"x"}`); w.Code != 503 || !strings.Contains(w.Body.String(), "different base domain") {
		t.Fatalf("wrong ingress response: %d %s", w.Code, w.Body.String())
	}
	if w := serve(a, "GET", "/stacks/interrupted", ""); w.Code != 200 {
		t.Fatalf("status on wrong ingress: %d %s", w.Code, w.Body.String())
	}
}

func TestFailureFencesRouteWhileLedgerWriteIsUnavailable(t *testing.T) {
	a, r := setupAPI(t)
	ctx := t.Context()
	db, err := sql.Open("sqlite", filepath.Join(a.cfg.StateDir, "stacks.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(ctx, `CREATE TRIGGER fail_terminal BEFORE UPDATE OF state ON stacks
		WHEN NEW.state IN ('ready', 'failed') BEGIN SELECT RAISE(FAIL, 'terminal writes unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	id := postID(t, a, `{"inline_yaml":"x"}`)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if published, aborted := r.counts(); published == 1 && aborted == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("route was not fenced while writes were unavailable")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_terminal`); err != nil {
		t.Fatal(err)
	}
	s := waitState(t, a, id, "failed")
	if s.FailurePhase != "ready" {
		t.Fatal(s)
	}
}

func TestRecoveryWaitsForConfiguredIngress(t *testing.T) {
	a, _ := setupAPI(t)
	ctx := t.Context()
	cfg := config.Config{StateDir: a.cfg.StateDir}
	if err := a.db.CreateAPI(ctx, "interrupted", "interrupted.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Pending(ctx, "interrupted"); err != nil {
		t.Fatal(err)
	}
	handler, err := NewAPI(ctx, cfg, a.db)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", "/stacks/interrupted", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"failed"`) || !strings.Contains(w.Body.String(), `"publication_pending":true`) {
		t.Fatalf("status without ingress: %d %s", w.Code, w.Body.String())
	}

	socket := filepath.Join(t.TempDir(), "moved.sock")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			w.Header().Set("ETag", `"version"`)
			_, _ = w.Write([]byte("[]"))
			return
		}
		w.WriteHeader(204)
	})}
	go func() { _ = srv.Serve(listener) }()
	defer func() { _ = srv.Close() }()
	cfg.BaseDomain, cfg.CaddyAdminSocket = "example.test", socket
	handler, err = NewAPI(ctx, cfg, a.db)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", "/stacks/interrupted", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"failed"`) || strings.Contains(w.Body.String(), "publication_pending") {
		t.Fatalf("recovered status: %d %s", w.Code, w.Body.String())
	}
}
func TestDefaultDurationAndAcquisitionFailure(t *testing.T) {
	a, _ := setupAPI(t)
	id := postID(t, a, `{"inline_yaml":"x"}`)
	s := waitState(t, a, id, "ready")
	ready, _ := time.Parse(time.RFC3339Nano, s.ReadyAt)
	expires, _ := time.Parse(time.RFC3339Nano, s.ExpiresAt)
	if expires.Sub(ready) != 168*time.Hour {
		t.Fatal("wrong default duration")
	}
	id = postID(t, a, `{"reference":"/no/such/file"}`)
	s = waitState(t, a, id, "failed")
	if s.FailurePhase != "acquisition" {
		t.Fatal(s)
	}
}

func TestEmptyInlineFailsComposeAndCommittedReadySurvivesFailure(t *testing.T) {
	a, r := setupAPI(t)
	a.launch = podman.Launch
	id := postID(t, a, `{"inline_yaml":""}`)
	s := waitState(t, a, id, "failed")
	if s.FailurePhase != "startup" {
		t.Fatalf("empty compose: %+v", s)
	}
	m, err := a.db.GetManifest(t.Context(), id)
	if err != nil || m.Content == nil || len(m.Content) != 0 {
		t.Fatalf("empty manifest: %+v %v", m, err)
	}
	id = "committed"
	host := id + ".example.test"
	if err := a.db.CreateAPI(t.Context(), id, host, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Pending(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Ready(t.Context(), id, "https://"+host, nil, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	a.fail(id, host, "ready", errors.New("lost database acknowledgement"), true)
	s, err = a.db.Status(t.Context(), id)
	if err != nil || s.State != "ready" {
		t.Fatalf("committed result: %+v %v", s, err)
	}
	if _, aborted := r.counts(); aborted != 0 {
		t.Fatal("committed route was aborted")
	}
}

func TestHandlerAvailableWithoutIngress(t *testing.T) {
	a, _ := setupAPI(t)
	if err := a.db.Create(t.Context(), "old", "old.example.test", time.Hour); err != nil {
		t.Fatal(err)
	}
	handler, err := NewAPI(t.Context(), config.Config{StateDir: a.cfg.StateDir}, a.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/healthz", "", 204}, {"GET", "/stacks/old", "", 200}, {"POST", "/stacks", `{"inline_yaml":"x"}`, 503},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}
