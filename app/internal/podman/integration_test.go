package podman

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

// TestRootlessPodman is deliberately opt-in because it pulls an image and creates resources.
func TestRootlessPodman(t *testing.T) {
	if os.Getenv("STACK_LAUNCHER_PODMAN_TEST") != "1" {
		t.Skip("set STACK_LAUNCHER_PODMAN_TEST=1 to run real rootless Podman integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	db, err := ledger.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
	checkout := filepath.Join(dir, "checkouts", "pinned")
	if err = os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"config.txt": "config from checkout", "secret.txt": "secret from checkout"} {
		if err = os.WriteFile(filepath.Join(checkout, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture"}} {
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed test fixture arguments
		cmd.Dir = checkout
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = checkout
	hash, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "env", "defaults"), []byte("ADMIN_VALUE=admin\nOVERRIDE_VALUE=default\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := `services:
  web:
    image: docker.io/library/nginx:1-alpine
    depends_on: [zhelper]
    env_file: defaults
    x-launcher:
      launch_command: [sh, -c, 'test -f /data/prerequisite && test "$$ADMIN_VALUE" = admin && test "$$OVERRIDE_VALUE" = saved && n=$$(cat /data/count 2>/dev/null || echo 0) && echo $$((n + 1)) > /data/count']
    ports: ['81:81']
    volumes: ['data:/data']
    configs: [{source: c, target: /etc/launcher-config}]
    secrets: [{source: s, target: /run/secrets/launcher-secret}]
    healthcheck: {test: [CMD-SHELL, 'wget -q -O /dev/null http://127.0.0.1/'], interval: 1s, timeout: 2s, retries: 10}
  zhelper:
    entrypoint: [sh, -c]
    command: ['sleep 3600']
    environment: {ROLE: helper}
    stop_grace_period: 1s
    volumes: ['data:/data']
    x-launcher:
      launch_command: [sh, -c, 'test "$$ROLE" = helper && echo prerequisite > /data/prerequisite']
volumes: {data: {}}
configs: {c: {file: ./config.txt}}
secrets: {s: {file: ./secret.txt}}
x-launcher:
  endpoint: {service: web, port: 80, https: false}
`
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...).CombinedOutput() // #nosec G204 -- test-generated project resources
		if err != nil {
			t.Fatalf("podman %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	projects := []string{}
	ids := []string{}
	defer func() {
		for _, project := range projects {
			cleanupCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
			for _, target := range []struct{ resource, service string }{{"container", "web"}, {"container", "zhelper"}, {"network", ""}, {"volume", ""}} {
				resource := target.resource
				args := []string{"--remote=false", resource, "ls", "-q", "--filter", "label=com.docker.compose.project=" + project}
				if resource == "container" {
					args = append(args, "-a", "--filter", "label=com.docker.compose.service="+target.service)
				}
				cmd := exec.CommandContext(cleanupCtx, "podman", args...) // #nosec G204 -- scoped to test-generated project resources
				out, _ := cmd.Output()
				ids := strings.Fields(string(out))
				if len(ids) > 0 {
					args := append([]string{"--remote=false", resource, "rm", "-f"}, ids...)
					if output, err := exec.CommandContext(cleanupCtx, "podman", args...).CombinedOutput(); err != nil { // #nosec G204 -- scoped to test-generated project resources
						t.Errorf("cleanup %s %s: %v: %s", project, resource, err, output)
					}
				}
			}
			c()
		}
	}()
	results := make([]Result, 0, 2)
	saved := make([]ledger.Image, 0, 2)
	networkNames := make([]string, 0, 2)
	volumeNames := make([]string, 0, 2)
	overrides := map[string]string{"zhelper": "docker.io/library/nginx:1-alpine"}
	for i := 0; i < 2; i++ {
		id := fmt.Sprintf("integration-%d-%d", time.Now().UnixNano(), i)
		if err = db.CreateAPIWithEnvironment(ctx, id, fmt.Sprintf("integration-%d.example", i), time.Second, ledger.Environment{"web": {"OVERRIDE_VALUE": "saved"}}); err != nil {
			t.Fatal(err)
		}
		fixture := ledger.Manifest{Content: []byte(manifest), Checkout: "pinned", Path: "compose.yaml", Commit: strings.TrimSpace(string(hash))}
		if err = db.SaveManifest(ctx, id, fixture); err != nil {
			t.Fatal(err)
		}
		// Derive the project before launch so a failed launch is also cleaned up.
		ids = append(ids, id)
		prepared, err := compose.PrepareWithImages(dir, id, fixture, overrides)
		if err != nil {
			t.Fatal(err)
		}
		projects = append(projects, prepared.ProjectName)
		result, err := Launch(ctx, config.Config{StateDir: dir}, db, id, overrides)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
		images, err := db.GetImages(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, images["web"])
		containerID := run("ps", "-aq", "--filter", "label=com.docker.compose.project="+result.ProjectName, "--filter", "label=com.docker.compose.service=web")
		if got := run("exec", containerID, "cat", "/data/count"); got != "1" {
			t.Fatalf("fresh launch seed count: %q", got)
		}
		run("exec", containerID, "sh", "-c", "echo retained >/data/marker")
		if got := run("exec", containerID, "cat", "/etc/launcher-config"); got != "config from checkout" {
			t.Fatalf("config: %q", got)
		}
		if got := run("exec", containerID, "cat", "/run/secrets/launcher-secret"); got != "secret from checkout" {
			t.Fatalf("secret: %q", got)
		}
		actual := run("inspect", "--format", "{{.Image}}", containerID)
		if strings.TrimPrefix(actual, "sha256:") != images["web"].ID {
			t.Fatalf("image %s != %s", actual, images["web"].ID)
		}
		networkNames = append(networkNames, run("network", "ls", "-q", "--filter", "label=com.docker.compose.project="+result.ProjectName))
		volumeNames = append(volumeNames, run("volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+result.ProjectName))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+result.Endpoint.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP: %s", resp.Status)
		}
		if len(result.Bindings["web"]) != 2 {
			t.Fatalf("bindings: %v", result.Bindings)
		}
	}
	if results[0].ProjectName == results[1].ProjectName || results[0].Endpoint == results[1].Endpoint || results[0].Bindings["web"][81] == results[1].Bindings["web"][81] || networkNames[0] == "" || networkNames[1] == "" || networkNames[0] == networkNames[1] || volumeNames[0] == "" || volumeNames[1] == "" || volumeNames[0] == volumeNames[1] {
		t.Fatalf("isolation: %+v networks=%v volumes=%v", results, networkNames, volumeNames)
	}

	failedID := fmt.Sprintf("integration-failed-%d", time.Now().UnixNano())
	failedManifest := `services:
  web:
    image: docker.io/library/nginx:1-alpine
    command: [sh, -c, 'exit 7']
    volumes: ['data:/data']
volumes: {data: {}}
x-launcher:
  endpoint: {service: web, port: 80, https: false}
`
	failedFixture := ledger.Manifest{Content: []byte(failedManifest)}
	if err = db.Create(ctx, failedID, "failed.example", time.Second); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveManifest(ctx, failedID, failedFixture); err != nil {
		t.Fatal(err)
	}
	failedPrepared, err := compose.Prepare(dir, failedID, failedFixture)
	if err != nil {
		t.Fatal(err)
	}
	projects = append(projects, failedPrepared.ProjectName)
	if _, err = Launch(ctx, config.Config{StateDir: dir}, db, failedID, nil); err == nil {
		t.Fatal("failing container reported ready")
	}
	if got := run("ps", "-aq", "--filter", "label=com.docker.compose.project="+failedPrepared.ProjectName); got != "" {
		t.Fatalf("failed containers remain: %s", got)
	}
	if got := run("network", "ls", "-q", "--filter", "label=com.docker.compose.project="+failedPrepared.ProjectName); got != "" {
		t.Fatalf("failed networks remain: %s", got)
	}
	if got := run("volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+failedPrepared.ProjectName); got == "" {
		t.Fatal("failed project volume was deleted")
	}
	for range 2 {
		if err = Stop(ctx, config.Config{StateDir: dir}, db, ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	projectFilter := "label=com.docker.compose.project=" + projects[0]
	if got := run("ps", "-aq", "--filter", projectFilter); got != "" {
		t.Fatalf("stopped containers remain: %s", got)
	}
	if got := run("network", "ls", "-q", "--filter", projectFilter); got != "" {
		t.Fatalf("stopped networks remain: %s", got)
	}
	mountpoint := run("volume", "inspect", "--format", "{{.Mountpoint}}", volumeNames[0])
	if got := run("unshare", "cat", filepath.Join(mountpoint, "marker")); got != "retained" {
		t.Fatalf("stopped volume data: %q", got)
	}
	if _, err = Start(ctx, config.Config{StateDir: dir}, db, ids[0]); err != nil {
		t.Fatal(err)
	}
	restartedID := run("ps", "-aq", "--filter", projectFilter, "--filter", "label=com.docker.compose.service=web")
	if got := run("exec", restartedID, "cat", "/data/count"); got != "1" {
		t.Fatalf("restart reran seed: %q", got)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := ledger.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	for i, id := range ids {
		images, err := reopened.GetImages(ctx, id)
		if err != nil || images["web"] != saved[i] {
			t.Fatalf("reopened images: %v %v", images, err)
		}
	}

	// Hold one volume from outside the project so the first Remove only partly succeeds.
	blocker := run("create", "--pull=never", "--network=none", "--volume", volumeNames[0]+":/held:ro", "docker.io/library/nginx:1-alpine")
	defer cleanupPodman("rm", "--force", "--ignore", "--", blocker)
	cacheDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheDir, "Containerfile"), []byte("FROM scratch\nCOPY marker /marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "marker"), []byte(ids[0]), 0600); err != nil {
		t.Fatal(err)
	}
	cacheImage := "localhost/" + ids[0] + "-cache:latest"
	run("build", "--pull=never", "--layers", "--tag", cacheImage, cacheDir)
	defer cleanupPodman("rmi", "--", cacheImage)

	if err := Remove(ctx, ids[0]); err == nil || !strings.Contains(err.Error(), "remove volume") {
		t.Fatalf("expected blocked volume after containers and networks were removed: %v", err)
	}
	if got := run("ps", "-aq", "--filter", projectFilter); got != "" {
		t.Fatalf("containers remain after partial removal: %s", got)
	}
	if got := run("network", "ls", "-q", "--filter", projectFilter); got != "" {
		t.Fatalf("networks remain after partial removal: %s", got)
	}
	run("container", "exists", blocker)
	run("rm", "--", blocker)
	for _, id := range []string{ids[0], ids[0], failedID} {
		if err := Remove(ctx, id); err != nil {
			t.Fatalf("remove %s: %v", id, err)
		}
		if got := run("volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+compose.ProjectName(id)); got != "" {
			t.Fatalf("volumes remain for %s: %s", id, got)
		}
	}
	otherID := run("ps", "-q", "--filter", "label=com.docker.compose.project="+projects[1], "--filter", "label=com.docker.compose.service=web")
	if got := run("exec", otherID, "cat", "/data/marker"); got != "retained" {
		t.Fatalf("other stack volume data: %q", got)
	}
	run("network", "exists", networkNames[1])
	run("volume", "exists", volumeNames[1])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+results[1].Endpoint.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("other project after removal: %s", resp.Status)
	}
	run("image", "exists", cacheImage)
	if got := run("build", "--pull=never", "--layers", "--tag", cacheImage, cacheDir); !strings.Contains(got, "Using cache") {
		t.Fatalf("build cache was not reused: %s", got)
	}
}

func cleanupPodman(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...).Run() // #nosec G204 -- test-created resources
}
