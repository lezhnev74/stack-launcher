package caddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockAdmin struct {
	mu         sync.Mutex
	routes     []json.RawMessage
	version    int
	conflicts  int
	status     int
	patchError int
	missingTag bool
	drop       bool
}

func (m *mockAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status != 0 {
		w.WriteHeader(m.status)
		return
	}
	tag := fmt.Sprintf(`"%d"`, m.version)
	if r.Method == http.MethodGet {
		if !m.missingTag {
			w.Header().Set("ETag", tag)
		}
		_ = json.NewEncoder(w).Encode(m.routes)
		return
	}
	if m.patchError != 0 {
		w.WriteHeader(m.patchError)
		return
	}
	if m.conflicts > 0 {
		m.conflicts--
		m.routes = append(m.routes, json.RawMessage(fmt.Sprintf(`{"@id":"concurrent-%d","handle":[]}`, m.version)))
		m.version++
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if r.Header.Get("If-Match") != tag {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&m.routes); err != nil {
		w.WriteHeader(400)
		return
	}
	m.version++
	if m.drop {
		m.drop = false
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
}
func testRouter(t *testing.T, m *mockAdmin) *Router {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	router, err := New("example.com", socket)
	if err != nil {
		t.Fatal(err)
	}
	return router
}
func TestRoutes(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{json.RawMessage(`{"handle":[{"handler":"static_response","status_code":404}]}`)}, conflicts: 1}
	r := testRouter(t, m)
	ctx := context.Background()
	addr := netip.MustParseAddrPort("127.0.0.1:8080")
	if err := r.Publish(ctx, "ONE.Example.com", addr, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(ctx, "two.example.com", addr, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(ctx, "one.example.com", addr, true); err != nil {
		t.Fatal(err)
	}
	if len(m.routes) != 4 {
		t.Fatalf("routes: %s", m.routes)
	}
	if !strings.Contains(string(m.routes[1]), "https://one.example.com{http.request.uri}") {
		t.Fatal(string(m.routes[1]))
	}
	if err := r.Withdraw(ctx, "one.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := r.Withdraw(ctx, "one.example.com"); err != nil {
		t.Fatal(err)
	}
	if len(m.routes) != 3 || !strings.Contains(string(m.routes[0]), "two.example.com") || !strings.Contains(string(m.routes[2]), `"@id":"concurrent-0"`) {
		t.Fatalf("routes: %s", m.routes)
	}
}

func TestPublishLeavesRoomForRecovery(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{json.RawMessage(`{"handle":[],"pad":"` + strings.Repeat("x", maxRoutesBytes-250) + `"}`)}}
	router := testRouter(t, m)
	ctx := t.Context()
	if err := router.Publish(ctx, "one.example.com", netip.MustParseAddrPort("127.0.0.1:8080"), true); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("publish past the route size limit: %v", err)
	}
	if err := router.Abort(ctx, "one.example.com"); err != nil {
		t.Fatalf("publication fence no longer fits: %v", err)
	}
}
func TestValidation(t *testing.T) {
	for _, s := range []string{"", "*.example.com", "127.0.0.1", "example.com.", "http://example.com", "a..b", "-a.example"} {
		if ValidDomain(s) {
			t.Errorf("accepted %q", s)
		}
	}
	if _, err := New("bad.", "socket"); err == nil {
		t.Fatal("invalid domain")
	}
	if _, err := New("example.com", "http://localhost"); err == nil {
		t.Fatal("invalid socket")
	}
	r := testRouter(t, &mockAdmin{routes: []json.RawMessage{}})
	for _, host := range []string{"example.com", "a.b.example.com", "a.other.com", "*.example.com", "K.example.com"} {
		if err := r.Withdraw(context.Background(), host); err == nil {
			t.Errorf("accepted %q", host)
		}
	}
	if err := r.Publish(context.Background(), "a.example.com", netip.MustParseAddrPort("127.0.0.2:80"), true); err == nil {
		t.Fatal("invalid address")
	}
	if err := r.Publish(context.Background(), "a.example.com", netip.MustParseAddrPort("127.0.0.1:0"), true); err == nil {
		t.Fatal("invalid port")
	}
}
func TestErrors(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{}, missingTag: true}
	r := testRouter(t, m)
	if err := r.Withdraw(context.Background(), "a.example.com"); err == nil {
		t.Fatal("missing etag")
	}
	m.missingTag = false
	m.status = 500
	if err := r.Withdraw(context.Background(), "a.example.com"); err == nil {
		t.Fatal("status")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Withdraw(ctx, "a.example.com"); err == nil {
		t.Fatal("cancellation")
	}
	m.status = 0
	m.routes = nil
	if err := r.Withdraw(context.Background(), "a.example.com"); err == nil {
		t.Fatal("null routes")
	}
}

func TestRouteReplacementAndFailures(t *testing.T) {
	foreignRoute := json.RawMessage(`{"@id":123,"handle":[]}`)
	m := &mockAdmin{routes: []json.RawMessage{foreignRoute}}
	r := testRouter(t, m)
	ctx := context.Background()
	a := netip.MustParseAddrPort("127.0.0.1:1")
	if err := r.Publish(ctx, "a.example.com", a, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(ctx, "a.example.com", a, false); err != nil {
		t.Fatal(err)
	}
	if len(m.routes) != 2 || strings.Contains(string(m.routes[0]), "308") || string(m.routes[1]) != string(foreignRoute) {
		t.Fatalf("routes: %s", m.routes)
	}
	m.routes = []json.RawMessage{json.RawMessage(`[]`)}
	if err := r.Withdraw(ctx, "a.example.com"); err == nil {
		t.Fatal("invalid route")
	}
	m.routes = []json.RawMessage{}
	m.conflicts = 1
	if err := r.Publish(ctx, "a.example.com", a, true); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedPatch(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{}, conflicts: retries}
	r := testRouter(t, m)
	ctx := context.Background()
	backend := netip.MustParseAddrPort("127.0.0.1:8080")
	if err := r.Publish(ctx, "a.example.com", backend, true); err == nil || !strings.Contains(err.Error(), "changed repeatedly") {
		t.Fatalf("exhausted conflict retries: %v", err)
	}
	m.patchError = http.StatusBadRequest
	if err := r.Publish(ctx, "a.example.com", backend, true); err == nil || !strings.Contains(err.Error(), "PATCH routes: HTTP 400") {
		t.Fatalf("rejected patch: %v", err)
	}
	for _, route := range m.routes {
		if strings.Contains(string(route), "a.example.com") {
			t.Fatalf("rejected route was published: %s", route)
		}
	}
}

func TestAppliedUnacknowledgedWithdrawal(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{}}
	r := testRouter(t, m)
	ctx := context.Background()
	if err := r.Publish(ctx, "a.example.com", netip.MustParseAddrPort("127.0.0.1:80"), true); err != nil {
		t.Fatal(err)
	}
	m.drop = true
	if err := r.Withdraw(ctx, "a.example.com"); err == nil {
		t.Fatal("expected lost response")
	}
	if err := r.Withdraw(ctx, "a.example.com"); err != nil {
		t.Fatal(err)
	}
	if len(m.routes) != 0 {
		t.Fatal("route remained")
	}
}

func TestAbortFencesStalePublication(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{json.RawMessage(`{"@id":123,"handle":[]}`)}}
	r := testRouter(t, m)
	ctx := context.Background()
	routes, tag, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delayed, err := makeRoute("a.example.com", "127.0.0.1:80", false)
	if err != nil {
		t.Fatal(err)
	}
	routes, _, err = change(routes, routeID("a.example.com"), delayed)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Abort(ctx, "a.example.com"); err != nil {
		t.Fatal(err)
	}
	status, err := r.patch(ctx, tag, routes)
	if err != nil || status != http.StatusPreconditionFailed {
		t.Fatalf("stale patch: %d %v", status, err)
	}
	if err := r.Publish(ctx, "b.example.com", netip.MustParseAddrPort("127.0.0.1:80"), true); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(ctx, "a.example.com", netip.MustParseAddrPort("127.0.0.1:81"), true); err != nil {
		t.Fatal(err)
	}
	m.conflicts = 1
	if err := r.Abort(ctx, "a.example.com"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var markers int
	for _, raw := range m.routes {
		if strings.Contains(string(raw), "a.example.com") {
			t.Fatal("route remains")
		}
		if strings.Contains(string(raw), "stack-launcher-revision-") {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("markers: %d", markers)
	}
}
func TestAbortErrors(t *testing.T) {
	m := &mockAdmin{routes: []json.RawMessage{}}
	r := testRouter(t, m)
	ctx := context.Background()
	if err := r.Abort(ctx, "bad"); err == nil {
		t.Fatal("invalid host")
	}
	m.patchError = 500
	if err := r.Abort(ctx, "a.example.com"); err == nil {
		t.Fatal("patch error")
	}
	m.patchError = 0
	m.conflicts = retries
	if err := r.Abort(ctx, "a.example.com"); err == nil {
		t.Fatal("conflicts")
	}
	m.status = 500
	if err := r.Abort(ctx, "a.example.com"); err == nil {
		t.Fatal("read error")
	}
	if _, err := replaceRevision([]json.RawMessage{json.RawMessage(`[]`)}, nil); err == nil {
		t.Fatal("invalid route")
	}
}
