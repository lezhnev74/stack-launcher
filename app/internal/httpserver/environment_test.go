package httpserver

import (
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/ledger"
)

func TestEnvironmentAPI(t *testing.T) {
	a, _ := setupAPI(t)
	if w := serve(a, "POST", "/stacks", `{"inline_yaml":"empty","environment":{"web":{"BAD=KEY":"value"}}}`); w.Code != 400 {
		t.Fatalf("invalid launch override: %d %s", w.Code, w.Body)
	}
	webRequest(t, a, "POST", "/ui/templates", map[string]string{"name": "environment", "inline_yaml": runManifest}, 201)
	id := createRun(t, a, "environment")
	path := "/stacks/" + id + "/environment"
	if w := serve(a, "PUT", path, `{"environment":{"web":{"ONE":"initial"}}}`); w.Code != 204 {
		t.Fatalf("save draft overrides: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "GET", path, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "initial") {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	for _, body := range []string{`{"environment":{"missing":{"A":"x"}}}`, `{"environment":{"web":{"BAD=KEY":"x"}}}`, `{"environment":{"web":{"A":null}}}`, `{"environment":null}`} {
		if w := serve(a, "PUT", path, body); w.Code != 400 {
			t.Errorf("put %s: %d", body, w.Code)
		}
	}
	if w := serve(a, "PUT", path, `{"environment":{"web":{"EMPTY":"","VALUE.WITH-DOTS":"$x"}}}`); w.Code != 204 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "GET", path, ""); strings.Contains(w.Body.String(), "initial") || !strings.Contains(w.Body.String(), "$x") {
		t.Fatalf("replace: %s", w.Body)
	}
	if w := serve(a, "PUT", path, `{"environment":{}}`); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := serve(a, "GET", path, ""); strings.TrimSpace(w.Body.String()) != `{"environment":{}}` {
		t.Fatal(w.Body.String())
	}
	if w := serve(a, "GET", "/stacks/missing/environment", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := serve(a, "PUT", "/stacks/missing/environment", `{"environment":{}}`); w.Code != 404 {
		t.Fatalf("replace for unknown stack: %d %s", w.Code, w.Body)
	}
	webRequest(t, a, "PUT", path, map[string]any{"environment": map[string]any{"web": map[string]string{"MODE": "draft"}}}, 204)
	webRequest(t, a, "POST", "/ui/runs/"+id+"/launch", map[string]any{}, 202)
	waitState(t, a, id, "ready")
	requirePage(t, a, "/ui/stacks?id="+id, "<dt><code>MODE</code></dt>", "<pre><span>draft</span></pre>")
}

func TestEnvironmentReadOnlyAfterLaunch(t *testing.T) {
	a, _ := setupAPI(t)
	const id = "failed-acquisition"
	const path = "/stacks/" + id + "/environment"
	const clear = `{"environment":{}}`
	const replace = `{"environment":{"web":{"VALUE":"replacement"}}}`
	if err := a.db.CreateAPIWithEnvironment(t.Context(), id, id+".example.test", time.Hour,
		ledger.Environment{"web": {"VALUE": "initial"}}); err != nil {
		t.Fatal(err)
	}
	if w := serve(a, "PUT", path, clear); w.Code != 409 {
		t.Fatalf("clear during launch: %d %s", w.Code, w.Body)
	}
	requirePage(t, a, "/ui/stacks?id="+id, "Read-only", "initial")
	if changed, err := a.db.Fail(t.Context(), id, "acquisition", "missing source"); err != nil || !changed {
		t.Fatalf("record failed acquisition: changed=%v err=%v", changed, err)
	}
	if w := serve(a, "PUT", path, replace); w.Code != 409 {
		t.Fatalf("replace without manifest: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "GET", path, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "initial") {
		t.Fatalf("saved overrides after rejected writes: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "PUT", path, clear); w.Code != 409 {
		t.Fatalf("clear without manifest: %d %s", w.Code, w.Body)
	}
	ready := postID(t, a, `{"inline_yaml":"services:\n  web:\n    image: example\nx-launcher:\n  endpoint: {service: web, port: 80}\n","environment":{"web":{"ONE":"launched"}}}`)
	waitState(t, a, ready, "ready")
	if w := serve(a, "PUT", "/stacks/"+ready+"/environment", replace); w.Code != 409 {
		t.Fatalf("replace after launch: %d %s", w.Code, w.Body)
	}
	if w := serve(a, "GET", "/stacks/"+ready+"/environment", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "launched") {
		t.Fatalf("saved overrides after launch: %d %s", w.Code, w.Body)
	}
}
