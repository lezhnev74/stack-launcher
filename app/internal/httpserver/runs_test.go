package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/podman"
)

const runManifest = `services:
  web:
    image: docker.io/library/nginx:1-alpine
  worker:
    build: .
    x-launcher:
      git: {repository: team/worker, ref: main}
x-launcher:
  endpoint: {service: web, port: 80}
`

func TestTemplateRunSnapshotAndLaunch(t *testing.T) {
	a, ingress := setupAPI(t)
	const name = "Demo & test.yml"
	template := map[string]any{
		"name": name, "inline_yaml": runManifest,
		"environment": map[string]any{"web": map[string]string{"APP_MODE": "</script><script>", "LEVEL": "default"}},
	}
	webRequest(t, a, "POST", "/ui/templates", template, 201)
	webRequest(t, a, "POST", "/ui/templates", template, 409)
	requirePage(t, a, "/ui/templates", "Demo &amp; test.yml", "/ui/templates/edit?name=Demo%20%26%20test.yml", "<code>web</code>", "<code>worker</code>")
	body := requirePage(t, a, "/ui/templates/edit?name=Demo%20%26%20test.yml", `"name":"worker"`, `"repository":"team/worker"`, `\u003c/script\u003e`)
	if strings.Contains(body, "</script><script>") {
		t.Fatal("template environment escaped the JSON script element")
	}
	id := createRun(t, a, name)
	detail := "/ui/stacks?id=" + id
	requirePage(t, a, "/ui", id, `data-state="draft"`)
	requirePage(t, a, detail, "Launch run", "Demo &amp; test.yml", `"kind":"git","value":"main"`)
	updated := strings.Replace(runManifest, "ref: main", "ref: stable", 1)
	webRequest(t, a, "PUT", "/ui/templates/Demo%20%26%20test.yml", map[string]any{
		"inline_yaml": updated, "environment": map[string]any{"web": map[string]string{"APP_MODE": "new"}},
	}, 204)
	requirePage(t, a, detail, `"value":"main"`, `"LEVEL":"default"`)
	requirePage(t, a, "/ui/stacks?id="+createRun(t, a, name), `"value":"stable"`, `"APP_MODE":"new"`)

	runPath := "/ui/runs/" + id
	webRequest(t, a, "PUT", runPath, map[string]string{"inline_yaml": runManifest + "# edited run manifest\n"}, 204)
	overrides := map[string]any{
		"environment": map[string]any{"web": map[string]string{"APP_MODE": "draft"}},
		"references": map[string]ledger.ReferenceOverride{
			"worker": {Kind: "git", Value: "feature/run"},
			"web":    {Kind: "image", Value: "docker.io/library/nginx:2"},
		},
	}
	webRequest(t, a, "PUT", runPath, overrides, 204)
	requirePage(t, a, detail, "feature/run", "docker.io/library/nginx:2", `"APP_MODE":"draft"`)
	overrides["environment"] = map[string]any{"web": map[string]string{"APP_MODE": "final"}}
	webRequest(t, a, "POST", runPath+"/launch", overrides, 202)
	status := waitState(t, a, id, "ready")
	if status.TemplateName != name || status.URL != "https://"+id+".example.test" {
		t.Fatalf("run metadata %+v", status)
	}
	if count, _ := ingress.counts(); count != 1 {
		t.Fatalf("published routes = %d", count)
	}
	requirePage(t, a, detail, status.URL, "127.0.0.1:1234", "feature/run", "docker.io/library/nginx:2")
	raw := serve(a, "GET", "/ui/stacks/manifest?id="+id, "")
	if raw.Code != 200 || raw.Body.String() != runManifest+"# edited run manifest\n" ||
		raw.Header().Get("Content-Type") != "text/plain" || raw.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("original manifest response: %d %v", raw.Code, raw.Header())
	}
	source, err := a.db.GetManifest(t.Context(), id)
	if err != nil || string(source.Content) != runManifest+"# edited run manifest\n" {
		t.Fatalf("run manifest: %s, %v", source.Content, err)
	}
	env, err := a.db.GetEnvironment(t.Context(), id)
	if err != nil || env["web"]["APP_MODE"] != "final" {
		t.Fatalf("run environment: %v, %v", env, err)
	}
	prepared, err := compose.PrepareRuntime(a.cfg.StateDir, id, source, nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Services["worker"].Build.Ref != "feature/run" || prepared.Services["web"].Image != "docker.io/library/nginx:2" ||
		!strings.Contains(string(prepared.YAML), "APP_MODE: final") {
		t.Fatalf("prepared project: %+v", prepared)
	}
	webRequest(t, a, "POST", runPath+"/launch", overrides, 409)
	webRequest(t, a, "PUT", runPath, overrides, 409)

	apiID := postID(t, a, `{"manifest_template":"Demo & test.yml","environment":{"web":{"EXTRA":"api"}}}`)
	waitState(t, a, apiID, "ready")
	env, err = a.db.GetEnvironment(t.Context(), apiID)
	if err != nil || env["web"]["APP_MODE"] != "new" || env["web"]["EXTRA"] != "api" {
		t.Fatalf("API inherited environment: %v, %v", env, err)
	}
}

func TestRunLaunchProgressAndRemoval(t *testing.T) {
	a, _ := setupAPI(t)
	webRequest(t, a, "POST", "/ui/templates", map[string]string{"name": "compose.yaml", "inline_yaml": runManifest}, 201)
	id := createRun(t, a, "compose.yaml")
	started, release := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() { close(release) })
	a.launch = func(ctx context.Context, _ config.Config, db *ledger.Ledger, id string, _ map[string]string) (podman.Result, error) {
		started <- db.SaveImages(ctx, id, map[string]ledger.Image{"web": {ID: strings.Repeat("a", 64)}})
		<-release
		return podman.Result{}, context.Canceled
	}
	path := "/ui/runs/" + id + "/launch"
	a.disabledReason = "launches require ingress"
	requirePage(t, a, "/ui/stacks?id="+id, a.disabledReason)
	webRequest(t, a, "POST", path, map[string]any{}, 503)
	a.disabledReason = ""
	webRequest(t, a, "POST", path, map[string]any{}, 202)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	requirePage(t, a, "/ui/stacks?id="+id, `data-state="launching"`, "Preparing the saved Compose manifest and environment.")
	if w := serve(a, "GET", "/ui/stacks/activity?id="+id, ""); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"has_manifest":true`) || !strings.Contains(w.Body.String(), "Preparing the saved Compose manifest") {
		t.Fatalf("live startup activity: %d %s", w.Code, w.Body.String())
	}
	if w := serve(a, "GET", "/stacks/"+id, ""); !strings.Contains(w.Body.String(), `"images_prepared":true`) {
		t.Fatalf("missing progress: %s", w.Body.String())
	}
	webRequest(t, a, "POST", path, map[string]any{}, 409)

	discard := createRun(t, a, "compose.yaml")
	if w := serve(a, "DELETE", "/stacks/"+discard, ""); w.Code != 204 {
		t.Fatalf("discard: %d %s", w.Code, w.Body.String())
	}
	if w := serve(a, "GET", "/ui/stacks?id="+discard, ""); w.Code != 404 {
		t.Fatalf("discarded draft: %d", w.Code)
	}
	// Let the worker finish while the test ledger and its context are still live.
	release <- struct{}{}
	waitState(t, a, id, "failed")
	requirePage(t, a, "/ui/stacks?id="+id, "Run activity", "context canceled", "Run failed in the startup phase.")
}

func TestTemplateRunValidation(t *testing.T) {
	a, _ := setupAPI(t)
	webRequest(t, a, "POST", "/ui/templates", map[string]string{"name": "compose.yml", "inline_yaml": runManifest}, 201)
	webRequest(t, a, "POST", "/ui/runs", map[string]string{"template": "missing.yaml"}, 404)
	webRequest(t, a, "POST", "/ui/runs", map[string]string{"template": ""}, 400)
	webRequest(t, a, "POST", "/ui/manifest-services", map[string]string{"inline_yaml": runManifest}, 200)
	webRequest(t, a, "POST", "/ui/manifest-services", map[string]string{"inline_yaml": "services: ["}, 400)
	webRequest(t, a, "PUT", "/ui/templates/unknown.yml", map[string]string{"inline_yaml": runManifest}, 404)
	webRequest(t, a, "PUT", "/ui/templates/compose.yml", map[string]string{"name": "folder/file", "inline_yaml": runManifest}, 400)

	id := createRun(t, a, "compose.yml")
	path := "/ui/runs/" + id
	for _, input := range []string{
		`{"environment":{"missing":{"KEY":"value"}}}`,
		`{"environment":{"web":{"":"value"}}}`,
		`{"references":{"web":{"kind":"git","value":"main"}}}`,
		`{"references":{"worker":{"kind":"image","value":"nginx"}}}`,
		`{"references":{"web":{"kind":"image","value":"https://example.com/web"}}}`,
		`{"references":{"web":{"kind":"git","value":"  "}}}`,
		`{"references":{"worker":{"kind":"other","value":"main"}}}`,
		`{"references":{"ghost":{"kind":"image","value":"nginx"}}}`,
		`{"inline_yaml":"services: ["}`,
	} {
		if w := serve(a, "PUT", path, input); w.Code != 400 {
			t.Errorf("invalid draft edit %s: %d %s", input, w.Code, w.Body.String())
		}
	}
	webRequest(t, a, "PUT", "/ui/runs/missing", map[string]string{"inline_yaml": runManifest}, 404)
	if w := serve(a, "GET", "/ui/templates/edit?name=missing.yaml", ""); w.Code != 404 {
		t.Fatalf("template editor: %d", w.Code)
	}
	requirePage(t, a, "/ui/templates/edit", "New template", "Update services")
	requirePage(t, a, "/ui/templates", "compose.yml")
}

func webRequest(t *testing.T, a *api, method, path string, value any, want int) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	w := serve(a, method, path, string(body))
	if w.Code != want {
		t.Fatalf("%s %s: got %d, want %d. %s", method, path, w.Code, want, w.Body.String())
	}
	return w.Body.Bytes()
}

func createRun(t *testing.T, a *api, name string) string {
	t.Helper()
	content := webRequest(t, a, "POST", "/ui/runs", map[string]string{"template": name}, 201)
	var result struct{ ID string }
	if err := json.Unmarshal(content, &result); err != nil || result.ID == "" {
		t.Fatalf("created run: %s, %v", content, err)
	}
	return result.ID
}

func requirePage(t *testing.T, a *api, path string, want ...string) string {
	t.Helper()
	w := serve(a, http.MethodGet, path, "")
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, body)
	}
	for _, marker := range want {
		if !strings.Contains(body, marker) {
			t.Errorf("GET %s: missing %q", path, marker)
		}
	}
	return body
}
