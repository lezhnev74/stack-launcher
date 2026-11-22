package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"stack-launcher.local/app/internal/ledger"
)

const base = `services:
  web:
    image: example/web:latest
x-launcher:
  endpoint:
    service: web
    port: 8080
`

func prepare(t *testing.T, content string) (Prepared, error) {
	t.Helper()
	return Prepare(t.TempDir(), "stack-1", ledger.Manifest{Content: []byte(content)})
}
func TestPrepareRegistry(t *testing.T) {
	source := `name: original
services:
  web:
    image: example/web:latest
    command: [sh, -c, 'echo $$HOME']
    healthcheck: {test: [CMD, 'true'], interval: 10s}
    depends_on: [db]
    environment: {EXPLICIT: value, EMPTY: ''}
    ports: ['8080:8080', '127.0.0.1:5432:5432/tcp', {target: 6000, published: '6000', protocol: tcp, host_ip: 127.0.0.1}]
    volumes: ['data:/data']
    networks: [internal]
  db:
    image: example/db
volumes: {data: {}}
networks: {internal: {}}
x-launcher:
  endpoint: {service: web, port: 8080}
`
	manifest := ledger.Manifest{Content: []byte(source)}
	first, err := Prepare(t.TempDir(), "a-b", manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(t.TempDir(), "a_b", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProjectName == second.ProjectName || first.Endpoint != (Endpoint{"web", 8080, true}) || string(manifest.Content) != source {
		t.Fatal("identity, endpoint, or source changed")
	}
	repeat, err := Prepare(t.TempDir(), "a-b", manifest)
	if err != nil || repeat.ProjectName != first.ProjectName {
		t.Fatalf("project identity is unstable: %v", err)
	}
	var root map[string]any
	if err = yaml.Unmarshal(first.YAML, &root); err != nil {
		t.Fatal(err)
	}
	services := root["services"].(map[string]any)
	web := services["web"].(map[string]any)
	ports := web["ports"].([]any)
	if len(ports) != 3 || root["name"] != first.ProjectName || root["x-launcher"] != nil || web["command"] == nil || web["healthcheck"] == nil || web["depends_on"] == nil || web["environment"] == nil || root["volumes"] == nil || root["networks"] == nil {
		t.Fatalf("unexpected launch copy: %s", first.YAML)
	}
	expected := map[string]any{"command": []any{"sh", "-c", "echo $$HOME"}, "healthcheck": map[string]any{"test": []any{"CMD", "true"}, "interval": "10s"}, "depends_on": []any{"db"}, "environment": map[string]any{"EXPLICIT": "value", "EMPTY": ""}, "volumes": []any{"data:/data"}, "networks": []any{"internal"}}
	for key, want := range expected {
		if !reflect.DeepEqual(web[key], want) {
			t.Errorf("%s = %#v, want %#v", key, web[key], want)
		}
	}
	if !reflect.DeepEqual(root["volumes"], map[string]any{"data": map[string]any{}}) || !reflect.DeepEqual(root["networks"], map[string]any{"internal": map[string]any{}}) {
		t.Fatal("named resources changed")
	}
	targets := map[int]bool{}
	for _, p := range ports {
		targets[p.(map[string]any)["target"].(int)] = true
	}
	if !reflect.DeepEqual(targets, map[int]bool{8080: true, 5432: true, 6000: true}) {
		t.Fatalf("targets %v", targets)
	}
	for _, p := range ports {
		m := p.(map[string]any)
		if m["host_ip"] != "127.0.0.1" || m["protocol"] != "tcp" || m["published"] != nil {
			t.Fatalf("bad port: %v", m)
		}
	}
}
func TestPrepareEndpoint(t *testing.T) {
	initial, err := prepare(t, base)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = yaml.Unmarshal(initial.YAML, &doc); err != nil {
		t.Fatal(err)
	}
	web := doc["services"].(map[string]any)["web"].(map[string]any)
	if !reflect.DeepEqual(web["ports"], []any{map[string]any{"target": 8080, "host_ip": "127.0.0.1", "protocol": "tcp"}}) {
		t.Fatalf("missing endpoint binding: %v", web["ports"])
	}

	p, err := prepare(t, strings.Replace(base, "port: 8080", "port: 8080\n    https: false", 1))
	if err != nil || p.Endpoint.HTTPS {
		t.Fatalf("endpoint: %+v %v", p.Endpoint, err)
	}
	for _, content := range []string{strings.Replace(base, "x-launcher:\n  endpoint:\n    service: web\n    port: 8080", "x-launcher: wrong", 1), strings.Replace(base, "service: web", "service: ''", 1), strings.Replace(base, "endpoint:\n    service: web\n    port: 8080", "endpoint: wrong", 1), strings.Replace(base, "service: web", "service: absent", 1), strings.Replace(base, "port: 8080", "port: 0", 1), strings.Replace(base, "port: 8080", "port: '8080'", 1), strings.Replace(base, "port: 8080", "port: 8080\n    https: yes", 1), strings.Replace(base, "port: 8080", "port: 8080\n    other: true", 1), strings.Replace(base, "x-launcher:", "x-launcher:\n  other: true", 1), strings.Replace(base, "x-launcher:", "x-launcher: {}\nx-extra:", 1)} {
		if _, err := prepare(t, content); err == nil {
			t.Errorf("accepted %s", content)
		}
	}
}
func TestPrepareRejectsUnsupported(t *testing.T) {
	cases := []string{"", "[]", "services: [", base + "---\n{}\n", strings.Replace(base, "image: example/web:latest", "image: one\n    image: two", 1), strings.Replace(base, "image: example/web:latest", "build: .", 1)}
	for _, field := range []string{"build: .", "env_file: .env", "container_name: shared", "network_mode: host", "profiles: [foo]", "extends: other", "x-launcher: {launch_command: [true]}", "provider: {}", "post_start: []", "privileged: true"} {
		cases = append(cases, strings.Replace(base, "image: example/web:latest", "image: example/web:latest\n    "+field, 1))
	}
	for _, field := range []string{"include: [other.yml]", "volumes: {data: {name: shared}}", "networks: {shared: {external: true}}", "secrets: {s: {environment: SECRET}}"} {
		cases = append(cases, base+field+"\n")
	}
	cases = append(cases, strings.Replace(base, "example/web:latest", "${IMAGE}", 1), strings.Replace(base, "image: example/web:latest", "image: x\n    environment: [SECRET]", 1), strings.Replace(base, "image: example/web:latest", "image: x\n    environment: {SECRET: null}", 1), strings.Replace(base, "image: example/web:latest", "image: x\n    environment: {SECRET: [bad]}", 1), strings.Replace(base, "image: example/web:latest", "image: x\n    environment: wrong", 1))
	for _, content := range cases {
		if _, err := prepare(t, content); err == nil {
			t.Errorf("accepted %q", content)
		}
	}
	if _, err := Prepare("", "", ledger.Manifest{Content: []byte(base)}); err == nil {
		t.Fatal("accepted empty ID")
	}
}
func TestPorts(t *testing.T) {
	for _, v := range []string{"80", "'80:81'", "'127.0.0.1:80:81/tcp'", "{target: 81, published: 80}", "'127.0.0.2:80:81'", "'[::1]:80:81'", "{target: 81, host_ip: '::1'}"} {
		content := strings.Replace(base, "image: example/web:latest", "image: x\n    ports: ["+v+"]", 1)
		if _, err := prepare(t, content); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	for _, v := range []string{"'0.0.0.0:80:81'", "'80:81/udp'", "'80-81'", "'+80'", "'0:80'", "'1:2:3:4'", "{target: 80, host_ip: '::2'}", "{target: 80, mode: host}", "{target: 80, protocol: udp}", "{target: 80, published: bad}", "{target: false}", "false", "65536"} {
		content := strings.Replace(base, "image: example/web:latest", "image: x\n    ports: ["+v+"]", 1)
		if _, err := prepare(t, content); err == nil {
			t.Errorf("accepted port %s", v)
		}
	}
	content := strings.Replace(base, "image: example/web:latest", "image: x\n    ports: bad", 1)
	if _, err := prepare(t, content); err == nil {
		t.Fatal("accepted nonlist ports")
	}
}
func TestPinnedPaths(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "checkouts", "checkout-1")
	dir := filepath.Join(root, "deploy", "nested")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	content := strings.Replace(base, "image: example/web:latest", "image: x\n    volumes: ['../file:/file:ro', {type: bind, source: ../file, target: /other, read_only: true}]", 1) + "configs: {c: {file: ../file}}\nsecrets: {s: {file: ../file}}\n"
	manifest := ledger.Manifest{Content: []byte(content), Checkout: "checkout-1", Path: "deploy/nested/compose.yaml"}
	result, err := Prepare(state, "id", manifest)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(result.YAML, &document); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, "deploy", "file")
	web := document["services"].(map[string]any)["web"].(map[string]any)
	wantMounts := []any{
		map[string]any{"type": "bind", "source": wantPath, "target": "/file", "read_only": true},
		map[string]any{"type": "bind", "source": wantPath, "target": "/other", "read_only": true},
	}
	if !reflect.DeepEqual(web["volumes"], wantMounts) || string(manifest.Content) != content {
		t.Fatalf("mount or source mismatch: %s", result.YAML)
	}
	for kind, name := range map[string]string{"configs": "c", "secrets": "s"} {
		resource := document[kind].(map[string]any)[name].(map[string]any)
		if resource["file"] != wantPath {
			t.Fatalf("%s file = %v; want %s", kind, resource["file"], wantPath)
		}
	}
	for _, mount := range []string{"'../file:/file'", "'../file:/file:rw'", "{type: bind, source: ../file, target: /file}", "{type: bind, source: ../file, target: /file, read_only: false}"} {
		writable := manifest
		writable.Content = []byte(strings.Replace(base, "image: example/web:latest", "image: x\n    volumes: ["+mount+"]", 1))
		if _, err := Prepare(state, "id", writable); err == nil || (!strings.Contains(err.Error(), "read-only") && !strings.Contains(err.Error(), "read_only")) {
			t.Errorf("writable bind %s: expected read-only error, got %v", mount, err)
		}
	}
	if err := os.Remove(filepath.Join(root, "deploy", "file")); err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(state, "id", manifest); err == nil {
		t.Fatal("accepted missing path")
	}
	if err = os.WriteFile(filepath.Join(root, "deploy", "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(state, "id", manifest); err != nil {
		t.Fatalf("retry: %v", err)
	}
	outside := filepath.Join(state, "outside")
	if err = os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"./escape", "../../../outside", "/etc/passwd", "./missing"} {
		m := manifest
		m.Content = []byte(strings.Replace(content, "../file", path, 1))
		if _, err := Prepare(state, "id", m); err == nil {
			t.Errorf("accepted path %s", path)
		}
	}
	m := manifest
	m.Checkout = ""
	if _, err := Prepare(state, "id", m); err == nil {
		t.Fatal("accepted path without checkout")
	}
}
func TestMountsAndMerges(t *testing.T) {
	for _, mount := range []string{"'./data:/data'", "'./data:/data:rw'", "{type: bind, source: ./data, target: /data, read_only: false}", "'/data'", "{type: volume, target: /data}", "{type: bind, source: ./data, target: /data, read_only: true, bind: {propagation: shared}}", "false", "'foo/bar:/data'", "'~/data:/data'", "{type: volume, source: foo/bar, target: /data}"} {
		content := strings.Replace(base, "image: example/web:latest", "image: x\n    volumes: ["+mount+"]", 1)
		if _, err := prepare(t, content); err == nil {
			t.Errorf("accepted mount %s", mount)
		}
	}
	content := `services:
  web:
    <<: &defaults
      image: web
      container_name: shared
x-launcher: {endpoint: {service: web, port: 80}}
`
	if _, err := prepare(t, content); err == nil {
		t.Fatal("merge bypassed service validation")
	}
	content = strings.Replace(content, "container_name: shared", "environment: {FOO: bar}", 1)
	if _, err := prepare(t, content); err != nil {
		t.Fatal(err)
	}
}

func TestDynamicLoopbackPort(t *testing.T) {
	for _, binding := range []string{"127.0.0.1::9090", "[::1]::9090"} {
		content := strings.Replace(base, "image: example/web:latest", "image: x\n    ports: ['"+binding+"']", 1)
		result, err := prepare(t, content)
		if err != nil {
			t.Fatalf("%s: %v", binding, err)
		}
		var doc map[string]any
		if err = yaml.Unmarshal(result.YAML, &doc); err != nil {
			t.Fatal(err)
		}
		web := doc["services"].(map[string]any)["web"].(map[string]any)
		want := []any{map[string]any{"target": 9090, "host_ip": "127.0.0.1", "protocol": "tcp"}, map[string]any{"target": 8080, "host_ip": "127.0.0.1", "protocol": "tcp"}}
		if !reflect.DeepEqual(web["ports"], want) {
			t.Fatalf("%s: %v", binding, web["ports"])
		}
	}
}

func TestResourceAndLiteralPaths(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state$literal")
	root := filepath.Join(state, "checkouts", "c")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	m := ledger.Manifest{Checkout: "c", Path: "compose.yml", Content: []byte(base + "configs: {c: {file: ./file}}\n")}
	result, err := Prepare(state, "id", m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.YAML), "state$$literal") {
		t.Fatalf("path was not escaped: %s", result.YAML)
	}
	for _, file := range []string{".", "./$$HOME"} {
		m.Content = []byte(base + "configs: {c: {file: '" + file + "'}}\n")
		if _, err = Prepare(state, "id", m); err == nil {
			t.Errorf("accepted config file %s", file)
		}
	}
	for _, checkout := range []string{"missing", "../c", ".", ".."} {
		m.Checkout = checkout
		m.Content = []byte(base + "configs: {c: {file: ./file}}\n")
		if _, err = Prepare(state, "id", m); err == nil {
			t.Errorf("accepted checkout %s", checkout)
		}
	}
	for _, suffix := range []string{"configs: {bad/name: {file: ./file}}\n", "volumes: {data: bad}\n", "configs: {c: {}}\n", "secrets: {s: bad}\n"} {
		if _, err = prepare(t, base+suffix); err == nil {
			t.Errorf("accepted %s", suffix)
		}
	}
}
func TestServiceReferences(t *testing.T) {
	state := t.TempDir()
	checkout := filepath.Join(state, "checkouts", "c")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "file"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := ledger.Manifest{Checkout: "c", Path: "compose.yaml"}
	const declarations = "configs: {c: {file: ./file}}\nsecrets: {s: {file: ./file}}\n"
	for _, entry := range []string{"configs: [c, {source: c, target: /etc/config}]", "secrets: [s, {source: s, target: /run/secret}]"} {
		text := strings.Replace(base, "image: example/web:latest", "image: x\n    "+entry, 1)
		manifest.Content = []byte(text + declarations)
		if _, err := Prepare(state, "id", manifest); err != nil {
			t.Errorf("%s: %v", entry, err)
		}
	}
	for _, entry := range []string{"configs: [missing]", "configs: [{source: missing}]", "secrets: [missing]", "secrets: [{source: missing}]"} {
		for _, definitions := range []string{"", declarations} {
			text := strings.Replace(base, "image: example/web:latest", "image: x\n    "+entry, 1)
			manifest.Content = []byte(text + definitions)
			if _, err := Prepare(state, "id", manifest); err == nil || !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), "not declared") {
				t.Errorf("undeclared %s: expected service and declaration error, got %v", entry, err)
			}
		}
	}
	for _, entry := range []string{"configs: wrong", "configs: [../file]", "secrets: [{source: s, external: true}]", "secrets: [false]", "environment: null", "environment: [1]"} {
		text := strings.Replace(base, "image: example/web:latest", "image: x\n    "+entry, 1)
		if _, err := prepare(t, text); err == nil {
			t.Errorf("accepted %s", entry)
		}
	}
}

func TestInterpolationErrorContext(t *testing.T) {
	for _, environment := range []string{"{TOKEN: '${PRIVATE_TOKEN}'}", "['TOKEN=${PRIVATE_TOKEN}']"} {
		content := strings.Replace(base, "image: example/web:latest", "image: x\n    environment: "+environment, 1)
		_, err := prepare(t, content)
		if err == nil {
			t.Fatalf("accepted interpolation: %s", environment)
		}
		message := err.Error()
		for _, field := range []string{"services", "web", "environment"} {
			if !strings.Contains(message, field) {
				t.Errorf("error missing %s context: %s", field, message)
			}
		}
		if strings.Contains(message, "PRIVATE_TOKEN") {
			t.Fatalf("error contains environment value: %s", message)
		}
	}
}

func TestImagesAndCompletion(t *testing.T) {
	source := `services:
  web:
    depends_on:
      job: {condition: service_completed_successfully}
  job: {image: registry/job}
x-launcher:
  endpoint: {service: web, port: 8080}
`
	manifest := ledger.Manifest{Content: []byte(source)}
	p, err := PrepareWithImages(t.TempDir(), "id", manifest, map[string]string{"web": "registry/web"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasCompletionDependency || p.Services["web"].Image != "registry/web" || !reflect.DeepEqual(p.Services["web"].Ports, []int{8080}) {
		t.Fatalf("services: %+v", p.Services)
	}
	for _, overrides := range []map[string]string{{"missing": "a"}, {"web": " "}} {
		if _, err := PrepareWithImages(t.TempDir(), "id", manifest, overrides); err == nil {
			t.Fatalf("accepted %v", overrides)
		}
	}
	id := strings.Repeat("a", 64)
	images := map[string]ledger.Image{"web": {ID: id}, "job": {ID: id}}
	output, err := p.PinImages(images)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "sha256:"+id) || !strings.Contains(string(output), "pull_policy: never") || strings.Contains(string(p.YAML), "pull_policy") {
		t.Fatalf("output: %s", output)
	}
	delete(images, "job")
	if _, err := p.PinImages(images); err == nil {
		t.Fatal("accepted missing image")
	}
	images["job"] = ledger.Image{ID: "invalid"}
	if _, err := p.PinImages(images); err == nil {
		t.Fatal("accepted invalid ID")
	}
	if _, err := prepare(t, strings.Replace(base, "image: example/web:latest", "image: example/web:latest\n    labels: {com.docker.compose.project: fake}", 1)); err == nil {
		t.Fatal("accepted reserved label")
	}
}

func TestPullPolicy(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  string
		valid bool
	}{
		{field: "", valid: true},
		{field: "pull_policy: always", valid: true},
		{field: "pull_policy: newer", want: "newer", valid: true},
		{field: "pull_policy: never"},
		{field: "pull_policy: null"},
	} {
		content := strings.Replace(base, "image: example/web:latest", "image: example/web:latest\n    "+tc.field, 1)
		p, err := prepare(t, content)
		if !tc.valid {
			if err == nil || !strings.Contains(err.Error(), `service "web": pull_policy`) {
				t.Fatalf("%s: expected a service-specific error, got %v", tc.field, err)
			}
			continue
		}
		if err != nil || p.Services["web"].PullPolicy != tc.want {
			t.Fatalf("%s: services %v, error %v", tc.field, p.Services, err)
		}
	}
	git := strings.Replace(base, "image: example/web:latest", "build: .\n    pull_policy: newer\n    x-launcher:\n      git: {repository: owner/web, ref: main}", 1)
	if _, err := prepare(t, git); err == nil || !strings.Contains(err.Error(), "pull_policy requires a registry image") {
		t.Fatalf("Git build accepted a pull policy: %v", err)
	}
}

func TestPinConfigsAndRepositoryBinds(t *testing.T) {
	state := t.TempDir()
	checkout := filepath.Join(state, "checkouts", "c")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(checkout, "file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	content := strings.Replace(base, "image: example/web:latest", "image: x\n    configs: [c, {source: c, target: /etc/config, mode: 0444}]\n    volumes: ['./file:/other:ro']", 1) + "configs: {c: {file: ./file}}\nsecrets: {s: {file: ./file}}\n"
	p, err := Prepare(state, "id", ledger.Manifest{Content: []byte(content), Checkout: "c", Path: "compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := p.PinImages(map[string]ledger.Image{"web": {ID: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := decode(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if root["configs"] != nil {
		t.Fatal("configs retained")
	}
	secret := root["secrets"].(map[string]any)["s"].(map[string]any)
	if secret["file"] != file || secret["x-podman.relabel"] != "z" || strings.Contains(string(p.YAML), "x-podman.relabel") {
		t.Fatalf("secret: %v", secret)
	}
	service := root["services"].(map[string]any)["web"].(map[string]any)
	mounts := service["volumes"].([]any)
	if len(mounts) != 3 {
		t.Fatalf("mounts: %v", mounts)
	}
	for i, target := range []string{"/other", "/c", "/etc/config"} {
		mount := mounts[i].(map[string]any)
		if mount["source"] != file || mount["target"] != target || mount["read_only"] != true || mount["bind"].(map[string]any)["selinux"] != "z" {
			t.Fatalf("mount: %v", mount)
		}
	}
	if !strings.Contains(string(p.YAML), "configs:") {
		t.Fatal("original changed")
	}
}

func TestLabelFormats(t *testing.T) {
	for _, label := range []string{"labels: [safe=value]", "labels: {safe: value}", "labels: [42, safe=value]"} {
		if _, err := prepare(t, strings.Replace(base, "image: example/web:latest", "image: x\n    "+label, 1)); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	for _, label := range []string{"labels: [io.podman.compose.project=fake]", "labels: invalid"} {
		if _, err := prepare(t, strings.Replace(base, "image: example/web:latest", "image: x\n    "+label, 1)); err == nil {
			t.Fatalf("accepted %s", label)
		}
	}
}
