package httpserver

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"stack-launcher.local/app/internal/compose"
)

func TestManifestTemplateUploadAndLaunch(t *testing.T) {
	a, _ := setupAPI(t)
	starter := serve(a, "GET", "/ui/manifest-example.yaml", "")
	if starter.Code != 200 || starter.Header().Get("Content-Disposition") != `attachment; filename="compose.yaml"` {
		t.Fatalf("starter download: %d %v", starter.Code, starter.Header())
	}
	const name = "Demo & test.yml"
	content := starter.Body.String() + "\n# keep the exact manifest bytes\n"
	if w := submitTemplate(t, a, name, content, "cross-site"); w.Code != 403 {
		t.Fatalf("cross-site upload: %d", w.Code)
	}
	if w := submitTemplate(t, a, name, content, "same-origin"); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if w := submitTemplate(t, a, name, starter.Body.String(), "same-origin"); w.Code != 409 {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	if w := submitTemplate(t, a, "broken.yml", "services: [", "same-origin"); w.Code != 400 {
		t.Fatalf("broken YAML: %d %s", w.Code, w.Body.String())
	}
	if w := submitTemplate(t, a, "demo.txt", content, "same-origin"); w.Code != 400 {
		t.Fatalf("wrong extension: %d %s", w.Code, w.Body.String())
	}
	if w := serve(a, "GET", "/ui", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `<option value="Demo &amp; test.yml">Demo &amp; test.yml</option>`) {
		t.Fatalf("template option: %d %s", w.Code, w.Body.String())
	}
	if w := serve(a, "POST", "/stacks", `{"manifest_template":"absent.yml"}`); w.Code != 404 {
		t.Fatalf("missing template: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{"manifest_template":"../private.yml"}`,
		`{"manifest_template":"Demo & test.yml","reference":"other.yml"}`,
		`{"manifest_template":"Demo & test.yml","inline_yaml":""}`,
	} {
		if w := serve(a, "POST", "/stacks", body); w.Code != 400 {
			t.Fatalf("bad launch %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	id := postID(t, a, `{"manifest_template":"Demo & test.yml"}`)
	waitState(t, a, id, "ready")
	manifest, err := a.db.GetManifest(t.Context(), id)
	if err != nil || string(manifest.Content) != content {
		t.Fatalf("saved launch manifest: %q, %v", manifest.Content, err)
	}
	detail := serve(a, "GET", "/ui/stacks?id="+id, "")
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `href="#service-panel-0">web</a>`) ||
		!strings.Contains(detail.Body.String(), "No saved overrides.") {
		t.Fatalf("missing service with no saved overrides: %d", detail.Code)
	}
}

func TestTemplateRename(t *testing.T) {
	a, _ := setupAPI(t)
	const original = "QA environment"
	const renamed = "Payments & QA"
	body := map[string]any{
		"name": original, "inline_yaml": strings.ReplaceAll(runManifest, "\n", "\r\n"),
		"environment": map[string]any{"web": map[string]string{"MODE": "review"}},
	}
	webRequest(t, a, "POST", "/ui/templates", body, 201)
	oldDraft := createRun(t, a, original)
	path := "/ui/templates/QA%20environment"
	webRequest(t, a, "POST", "/ui/templates", map[string]any{"name": "Occupied", "inline_yaml": runManifest}, 201)
	webRequest(t, a, "PUT", path, map[string]string{"name": "Occupied"}, 409)
	requirePage(t, a, "/ui/templates/edit?name=QA%20environment", `value="QA environment"`, "Compose manifest")
	webRequest(t, a, "PUT", path, map[string]string{"name": renamed}, 204)
	webRequest(t, a, "PUT", path, map[string]string{"name": renamed}, 404)

	saved, err := a.db.ManifestTemplate(t.Context(), renamed)
	if err != nil || string(saved.Content) != body["inline_yaml"] || saved.Environment["web"]["MODE"] != "review" {
		t.Fatalf("rename changed the saved configuration: %+v, %v", saved, err)
	}
	requirePage(t, a, "/ui", `<option value="Payments &amp; QA">Payments &amp; QA</option>`)
	requirePage(t, a, "/ui/stacks?id="+oldDraft, "<dt>Source template</dt><dd>QA environment</dd>")
	requirePage(t, a, "/ui/stacks?id="+createRun(t, a, renamed), "<dt>Source template</dt><dd>Payments &amp; QA</dd>")
	id := postID(t, a, `{"manifest_template":"Payments & QA"}`)
	waitState(t, a, id, "ready")
}

func TestServiceSummaryPorts(t *testing.T) {
	a, _ := setupAPI(t)
	const manifest = `services:
  web:
    image: nginx
    ports: ["127.0.0.1:9000:8080", 8080]
  store:
    image: redis
    ports:
      - target: 6379
        published: 6380
    expose: [6381, "6382/udp"]
  worker:
    image: nginx
x-launcher:
  endpoint: {service: web, port: 80}
`
	body := webRequest(t, a, "POST", "/ui/manifest-services", map[string]string{"inline_yaml": manifest}, 200)
	var services []compose.ServiceReference
	if err := json.Unmarshal(body, &services); err != nil || len(services) != 3 {
		t.Fatalf("service summary: %s (%v)", body, err)
	}
	store, web, worker := services[0], services[1], services[2]
	if store.Name != "store" || !slices.Equal(store.Ports, []int{6379}) ||
		!slices.Equal(store.Exposed, []string{"6381", "6382/udp"}) || store.HTTPPort != 0 {
		t.Errorf("store summary: %+v", store)
	}
	if web.Name != "web" || !slices.Equal(web.Ports, []int{8080, 80}) || web.HTTPPort != 80 || !web.HTTPS {
		t.Errorf("web summary: %+v", web)
	}
	if worker.Name != "worker" || len(worker.Ports) != 0 || len(worker.Exposed) != 0 || worker.HTTPPort != 0 {
		t.Errorf("worker summary: %+v", worker)
	}
	invalid := strings.Replace(manifest, "127.0.0.1:9000:8080", "9000:8080/udp", 1)
	webRequest(t, a, "POST", "/ui/manifest-services", map[string]string{"inline_yaml": invalid}, 400)
}

func submitTemplate(t *testing.T, a *api, name, content, site string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("manifest", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ui/manifest-templates", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", site)
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, req)
	return w
}
