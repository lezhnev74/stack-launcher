package httpserver

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

func TestWebPages(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := ledger.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	handler, err := NewAPI(ctx, config.Config{StateDir: dir}, db)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil))
		return w
	}
	if w := get("/ui"); w.Code != 200 || !strings.Contains(w.Body.String(), "No runs yet") {
		t.Fatalf("empty list: %d %s", w.Code, w.Body.String())
	}
	if w := get("/ui/stacks?id=missing"); w.Code != 404 {
		t.Fatalf("missing: %d", w.Code)
	}
	if err := db.Create(ctx, "..", "legacy.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAPI(ctx, "checkout", "checkout.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.Pending(ctx, "checkout"); err != nil {
		t.Fatal(err)
	}
	bindings := map[string]map[int]netip.AddrPort{
		"web":   {80: netip.MustParseAddrPort("127.0.0.1:10080")},
		"extra": {9000: netip.MustParseAddrPort("127.0.0.1:19000")},
	}
	expiresAt := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	if err := db.Ready(ctx, "checkout", "https://checkout.example", bindings, expiresAt.Add(-time.Hour), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAPI(ctx, "catalog", "catalog.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if changed, err := db.Fail(ctx, "catalog", "startup", "<script>alert('bad')</script>"); err != nil || !changed {
		t.Fatalf("record fixture failure: changed=%t, err=%v", changed, err)
	}
	list := get("/ui")
	if list.Code != 200 || list.Header().Get("Cache-Control") != "no-store" || list.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	legacyLink := regexp.MustCompile(`href="([^"]+)">\.\.</a>`).FindStringSubmatch(list.Body.String())
	if len(legacyLink) == 0 || legacyLink[1] != "/ui/stacks?id=.." {
		t.Fatalf("legacy link: %v", legacyLink)
	}
	if w := get(legacyLink[1]); w.Code != 200 || !strings.Contains(w.Body.String(), "legacy.example") {
		t.Fatalf("legacy: %d %s", w.Code, w.Body.String())
	}
	for _, id := range []string{"checkout", "catalog"} {
		api := get("/stacks/" + id)
		if api.Code != 200 {
			t.Fatalf("API status for %s: %d", id, api.Code)
		}
		var status ledger.Status
		if err := json.Unmarshal(api.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		page := get("/ui/stacks?id=" + id)
		body := page.Body.String()
		state := `data-state="` + status.State + `">` + status.State + `</span>`
		if page.Code != 200 {
			t.Fatalf("detail %s: %d %s", id, page.Code, body)
		}
		for _, value := range []string{status.ID, status.Hostname, state} {
			if !strings.Contains(body, value) || !strings.Contains(list.Body.String(), value) {
				t.Errorf("stack %s status absent from list or detail: %q", id, value)
			}
		}
		if id == "checkout" {
			checkReadyPage(t, status, list.Body.String(), body)
		} else if status.Failure == "" || !strings.Contains(body, "<dd>"+status.FailurePhase+"</dd>") ||
			!strings.Contains(body, "<pre>"+html.EscapeString(status.Failure)+"</pre>") || strings.Contains(body, "<script>") {
			t.Fatalf("failure escaping: %s", body)
		}
	}
	if err := db.BeginStop(ctx, "checkout"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ui", "/ui/stacks?id=checkout"} {
		body := get(path).Body.String()
		if !strings.Contains(body, `data-state="stopping"`) || strings.Contains(body, "127.0.0.1:19000") ||
			strings.Contains(body, `href="https://checkout.example"`) || strings.Contains(body, expiresAt.Format(time.RFC3339Nano)) {
			t.Fatalf("stale page %s: %s", path, body)
		}
	}
}

func checkReadyPage(t *testing.T, status ledger.Status, list, detail string) {
	t.Helper()
	if status.URL == "" || status.ExpiresAt == "" {
		t.Fatalf("ready status has no URL or expiry: %+v", status)
	}
	for _, value := range []string{`href="` + status.URL + `"`, `datetime="` + status.ExpiresAt + `"`} {
		if !strings.Contains(list, value) || !strings.Contains(detail, value) {
			t.Errorf("ready fact absent from list or detail: %q", value)
		}
	}
	var bindings map[string]map[int]string
	if err := json.Unmarshal(status.Bindings, &bindings); err != nil {
		t.Fatal(err)
	}
	if bindings["extra"][9000] != "127.0.0.1:19000" {
		t.Fatalf("missing API binding: %v", bindings)
	}
	compact := strings.Join(strings.Fields(detail), "")
	for svc, ports := range bindings {
		for port, address := range ports {
			row := fmt.Sprintf("<tr><td>%s</td><td>%d/tcp</td><td><code>%s</code></td></tr>", svc, port, address)
			if !strings.Contains(compact, row) {
				t.Errorf("detail missing binding for %s %d/tcp at %s", svc, port, address)
			}
		}
	}
}

func TestWebControlsAndOriginProtection(t *testing.T) {
	ctx := t.Context()
	db, err := ledger.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &api{ctx: ctx, db: db}
	handler := a.handler()
	for _, test := range []struct {
		state                      string
		owned, stop, start, remove bool
	}{
		{"launching", true, false, false, false},
		{"starting", true, false, false, false},
		{"ready", true, true, false, true},
		{"stopping", true, true, false, true},
		{"stopped", true, false, true, true},
		{"failed", true, false, false, true},
		{"removing", true, false, false, true},
		{"ready", false, false, false, false},
	} {
		t.Run(fmt.Sprintf("%s-%t", test.state, test.owned), func(t *testing.T) {
			status := ledger.Status{Stack: ledger.Stack{State: test.state}, APIOwned: test.owned}
			detail := makeStackDetail(status)
			w := httptest.NewRecorder()
			renderWeb(w, "detail.html", detail)
			for marker, want := range map[string]bool{`value="stop"`: test.stop, `value="start"`: test.start, `value="remove"`: test.remove, "Save overrides": false, "Original manifest": true} {
				if strings.Contains(w.Body.String(), marker) != want {
					t.Errorf("%s: expected %t", marker, want)
				}
			}
			if w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
				t.Error("missing frame protection")
			}
		})
	}
	for _, test := range []struct {
		origin, site string
		want         int
	}{
		{"https://evil.example", "cross-site", 403},
		{"http://example.com", "same-origin", 404},
		{"", "", 404},
	} {
		req := httptest.NewRequestWithContext(ctx, "POST", "http://example.com/stacks/missing/stop", nil)
		if test.origin != "" {
			req.Header.Set("Origin", test.origin)
		}
		if test.site != "" {
			req.Header.Set("Sec-Fetch-Site", test.site)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != test.want {
			t.Errorf("origin %q site %q: got %d want %d", test.origin, test.site, w.Code, test.want)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, "GET", "/ui/controller.js", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fetch(") {
		t.Fatalf("controller: %d", w.Code)
	}
}
