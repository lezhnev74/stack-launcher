package podman

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/registry"
)

const testImage = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testManifest = `services:
  web:
    image: example/web:latest
x-launcher:
  endpoint: {service: web, port: 8080, https: false}
`

func fakeRuntime(t *testing.T) (config.Config, *ledger.Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DIR/log"
env | grep -E '^(COMPOSE_.*|PODMAN_COMPOSE_.*|REGISTRY_AUTH_FILE|CONTAINER_HOST|CONTAINER_CONNECTION|DOCKER_HOST|DOCKER_CONTEXT)=' > "$FAKE_DIR/env"
case "$1" in
 info) echo "${FAKE_REMOTE:-false} ${FAKE_ROOTLESS:-true}" ;;
 pull) if [ -f "$FAKE_DIR/failpull" ]; then echo 'registry denied' >&2; exit 1; fi; echo sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
 build)
  if [ -f "$FAKE_DIR/failbuild" ]; then echo 'build failed' >&2; exit 1; fi
  while [ "$#" -gt 0 ]; do
   case "$1" in --file) dockerfile="$2"; shift;; --iidfile) iidfile="$2"; shift;; esac
   shift
  done
  cp "$dockerfile" "$FAKE_DIR/built-dockerfile"
  echo sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa > "$iidfile" ;;
 compose) while [ "$#" -gt 0 ]; do case "$1" in --project-name) project="$2"; shift;; --file) file="$2"; shift;; up) cp "$file" "$FAKE_DIR/pinned"; if [ -f "$FAKE_DIR/failup" ]; then echo 'up failed' >&2; exit 1; fi; rm -f "$FAKE_DIR/stopped";; down) cp "$file" "$FAKE_DIR/down-pinned"; if [ -f "$FAKE_DIR/faildown" ]; then exit 1; fi; touch "$FAKE_DIR/stopped";; esac; shift; done; echo "$project" > "$FAKE_DIR/project" ;;
 exec)
  if [ -f "$FAKE_DIR/sleepexec" ]; then sleep 30; fi
  if [ -f "$FAKE_DIR/failexec" ]; then echo secret >&2; exit 9; fi
  if [ -f "$FAKE_DIR/exitafterexec" ]; then touch "$FAKE_DIR/exited"; fi ;;
 ps) if [ ! -f "$FAKE_DIR/failup" ] && [ ! -f "$FAKE_DIR/stopped" ]; then echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; fi ;;
 network) : ;;
 container)
  project=$(cat "$FAKE_DIR/project")
  health=healthy
  status=running
  if [ -f "$FAKE_DIR/starting" ]; then health=starting; rm "$FAKE_DIR/starting"; fi
  if [ -f "$FAKE_DIR/exited" ]; then status=exited; fi
  printf '[{"Id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Config":{"Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"web"}},"State":{"Status":"%s","Health":{"Status":"%s"}},"NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"12345"}]}}}]' "$project" "$status" "$health" ;;
 *) exit 2 ;;
esac
`
	for _, name := range []string{"podman", "podman-compose"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil { // #nosec G306 -- fake executables must be executable
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", dir)
	db, err := ledger.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.Create(t.Context(), "one", "one.example", time.Second); err != nil {
		t.Fatal(err)
	}
	if err = db.SaveManifest(t.Context(), "one", ledger.Manifest{Content: []byte(testManifest)}); err != nil {
		t.Fatal(err)
	}
	return config.Config{StateDir: dir, RegistryAuthFile: "/tmp/auth file"}, db, dir
}

func TestLaunchRetryAndIsolation(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	for _, key := range []string{"COMPOSE_FILE", "PODMAN_COMPOSE_FOO", "REGISTRY_AUTH_FILE", "CONTAINER_HOST", "CONTAINER_CONNECTION", "DOCKER_HOST", "DOCKER_CONTEXT"} {
		t.Setenv(key, "unwanted")
	}
	if err := os.WriteFile(filepath.Join(dir, "failpull"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	overrides := map[string]string{"web": "example/override:latest"}
	_, err := Launch(t.Context(), cfg, db, "one", overrides)
	if err == nil || !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), "pull") || !strings.Contains(err.Error(), "example/override:latest") || !strings.Contains(err.Error(), "registry denied") {
		t.Fatalf("pull error: %v", err)
	}
	if _, err = db.GetImages(t.Context(), "one"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("images saved after failed pull: %v", err)
	}
	if err = os.Remove(filepath.Join(dir, "failpull")); err != nil {
		t.Fatal(err)
	}
	got, err := Launch(t.Context(), cfg, db, "one", overrides)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint.String() != "127.0.0.1:12345" || got.HTTPS {
		t.Fatalf("result: %+v", got)
	}
	images, err := db.GetImages(t.Context(), "one")
	if err != nil || images["web"].ID != testImage || images["web"].Reference != "example/override:latest" {
		t.Fatalf("images: %v %v", images, err)
	}
	pinned, err := os.ReadFile(filepath.Join(dir, "pinned")) // #nosec G304 -- test-owned temporary fixture
	if err != nil || !strings.Contains(string(pinned), "sha256:"+testImage) || !strings.Contains(string(pinned), "pull_policy: never") || strings.Contains(string(pinned), "example/override:latest") {
		t.Fatalf("pinned: %s %v", pinned, err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test-owned temporary fixture
	if !strings.Contains(string(log), "pull --quiet --authfile /tmp/auth file -- docker://example/override:latest") || !strings.Contains(string(log), "container inspect aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("commands: %s", log)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env")) // #nosec G304 -- test-owned temporary fixture
	for _, key := range []string{"COMPOSE_FILE=", "PODMAN_COMPOSE_FOO=", "REGISTRY_AUTH_FILE=", "CONTAINER_HOST=", "CONTAINER_CONNECTION=", "DOCKER_HOST=", "DOCKER_CONTEXT="} {
		if strings.Contains(string(env), key) {
			t.Errorf("ambient %s leaked", key)
		}
	}
	if !strings.Contains(string(env), "PODMAN_COMPOSE_PROVIDER="+filepath.Join(dir, "bin", "podman-compose")) {
		t.Fatalf("provider: %s", env)
	}
	if _, err = Launch(t.Context(), cfg, db, "one", nil); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("relaunch: %v", err)
	}
}

func TestRetryPinnedCleansUpAndRerunsLaunchCommands(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	const id = "retry-hook"
	ctx := t.Context()
	source := strings.Replace(testManifest, "\nx-launcher:", "\n    x-launcher:\n      launch_command: [echo, seed]\nx-launcher:", 1)
	if err := db.Create(ctx, id, "retry-hook.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveManifest(ctx, id, ledger.Manifest{Content: []byte(source)}); err != nil {
		t.Fatal(err)
	}
	failUp, failDown := filepath.Join(dir, "failup"), filepath.Join(dir, "faildown")
	if err := os.WriteFile(failUp, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Launch(ctx, cfg, db, id, nil); err == nil {
		t.Fatal("first launch should fail after saving its images")
	}
	if err := os.Remove(failUp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failDown, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RetryPinned(ctx, cfg, db, id); err == nil || !strings.Contains(err.Error(), "clean up previous launch") {
		t.Fatalf("retry with failed cleanup: %v", err)
	}
	if err := os.Remove(failDown); err != nil {
		t.Fatal(err)
	}

	result, err := RetryPinned(ctx, cfg, db, id)
	if err != nil || result.Endpoint.String() != "127.0.0.1:12345" {
		t.Fatalf("retry: %+v %v", result, err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test-owned temporary fixture
	if err != nil {
		t.Fatal(err)
	}
	commands := string(log)
	if strings.Count(commands, " up -d\n") != 2 || strings.Count(commands, "pull --quiet") != 1 ||
		!strings.Contains(commands, "exec -- "+testImage+" echo seed") ||
		strings.LastIndex(commands, " down\n") > strings.LastIndex(commands, " up -d\n") {
		t.Fatalf("retry did not clean up and reuse its images for a fresh launch:\n%s", commands)
	}
}

func TestPullCacheRespectsPolicy(t *testing.T) {
	cfg, _, dir := fakeRuntime(t)
	r, err := newRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const image = "example/web:latest"
	services := map[string]compose.Service{
		"a": {Image: image, PullPolicy: "newer"},
		"b": {Image: image, PullPolicy: "newer"},
		"c": {Image: image},
		"d": {Image: image},
	}
	images, err := prepareImages(t.Context(), r, cfg.StateDir, cfg.RegistryAuthFile, services, registry.ResolveDigest)
	if err != nil || len(images) != len(services) {
		t.Fatalf("image selection: %v %v", images, err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test-owned temporary fixture
	if err != nil {
		t.Fatal(err)
	}
	const expected = "pull --quiet --policy=newer --authfile /tmp/auth file -- docker://example/web:latest\n" +
		"pull --quiet --authfile /tmp/auth file -- docker://example/web:latest\n"
	if string(log) != expected {
		t.Fatalf("pull commands: %s", log)
	}
}

func TestLaunchFailureCleanupAndRootless(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		cfg, db, dir := fakeRuntime(t)
		if err := os.WriteFile(filepath.Join(dir, "failup"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Launch(t.Context(), cfg, db, "one", nil)
		if err == nil || !strings.Contains(err.Error(), "start project") {
			t.Fatalf("%v", err)
		}
		images, err := db.GetImages(t.Context(), "one")
		if err != nil || images["web"].ID != testImage {
			t.Fatalf("missing image selection after startup failure: %v %v", images, err)
		}
		log, _ := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test-owned temporary fixture
		if !strings.Contains(string(log), " --podman-rm-args=--volumes down\n") || strings.Contains(string(log), "volume rm") {
			t.Fatalf("cleanup: %s", log)
		}
	})
	t.Run("rootful", func(t *testing.T) {
		cfg, db, _ := fakeRuntime(t)
		t.Setenv("FAKE_ROOTLESS", "false")
		_, err := Launch(t.Context(), cfg, db, "one", nil)
		if err == nil || !strings.Contains(err.Error(), "rootless") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("provider", func(t *testing.T) {
		cfg, db, _ := fakeRuntime(t)
		cfg.ComposeProvider = "no-such-compose-provider"
		_, err := Launch(t.Context(), cfg, db, "one", nil)
		if err == nil || !strings.Contains(err.Error(), "provider") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("remote", func(t *testing.T) {
		cfg, db, _ := fakeRuntime(t)
		t.Setenv("FAKE_REMOTE", "true")
		_, err := Launch(t.Context(), cfg, db, "one", nil)
		if err == nil || !strings.Contains(err.Error(), "local and rootless") {
			t.Fatalf("%v", err)
		}
	})
}

func TestLaunchRelativeStateDir(t *testing.T) {
	cfg, db, _ := fakeRuntime(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateDir, err = filepath.Rel(cwd, cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Launch(t.Context(), cfg, db, "one", nil); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchRejectsCompletionDependencyBeforePodman(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	ctx := t.Context()
	const manifest = `services:
  web:
    image: example/web
    depends_on:
      setup: {condition: service_completed_successfully}
  setup:
    image: example/web
    ports: [9000]
x-launcher:
  endpoint: {service: web, port: 8080}
`
	if err := db.Create(ctx, "completion", "completion.example", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveManifest(ctx, "completion", ledger.Manifest{Content: []byte(manifest)}); err != nil {
		t.Fatal(err)
	}
	if _, err := Launch(ctx, cfg, db, "completion", nil); err == nil || !strings.Contains(err.Error(), "service_completed_successfully") {
		t.Fatalf("launch error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launch ran Podman: %v", err)
	}
}

func TestStopRetryUsesSavedOverrides(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	if _, err := Launch(t.Context(), cfg, db, "one", map[string]string{"web": "example/override:latest"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "faildown"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), cfg, db, "one"); err == nil {
		t.Fatal("expected failed teardown")
	}
	if err := os.Remove(filepath.Join(dir, "faildown")); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), cfg, db, "one"); err != nil {
		t.Fatal(err)
	}
	if err := Stop(t.Context(), cfg, db, "one"); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(filepath.Join(dir, "down-pinned")) // #nosec G304 -- test runtime writes under t.TempDir
	if err != nil || !strings.Contains(string(pinned), "sha256:"+testImage) || !strings.Contains(string(pinned), "pull_policy: never") {
		t.Fatalf("pinned: %s %v", pinned, err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test runtime writes under t.TempDir
	if err != nil || strings.Count(string(log), "pull --quiet") != 1 || strings.Count(string(log), "--podman-rm-args=--volumes down") != 3 {
		t.Fatalf("commands: %s %v", log, err)
	}
}

func TestStartUsesSavedImageOverride(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	ctx := t.Context()
	if _, err := Start(ctx, cfg, db, "one"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("start without saved images: %v", err)
	}
	if err := db.Create(ctx, "override", "override.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	manifest := strings.Replace(testManifest, "image: example/web:latest", "env_file: defaults", 1)
	if err := db.SaveManifest(ctx, "override", ledger.Manifest{Content: []byte(manifest)}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "env"), 0700); err != nil {
		t.Fatal(err)
	}
	defaults := filepath.Join(dir, "env", "defaults")
	if err := os.WriteFile(defaults, []byte("MODE=initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := Launch(ctx, cfg, db, "override", map[string]string{"web": "example/override:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Stop(ctx, cfg, db, "override"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(defaults); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(ctx, cfg, db, "override"); err == nil || !strings.Contains(err.Error(), `env_file "defaults"`) {
		t.Fatalf("start without env file: %v", err)
	}
	if err := os.WriteFile(defaults, []byte("MODE=updated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Start(ctx, cfg, db, "override")
	if err != nil || result.ProjectName != first.ProjectName || result.Endpoint != first.Endpoint {
		t.Fatalf("start: %+v %v", result, err)
	}
	pinned, err := os.ReadFile(filepath.Join(dir, "pinned")) // #nosec G304 -- test runtime writes under t.TempDir
	if err != nil || !strings.Contains(string(pinned), "sha256:"+testImage) || !strings.Contains(string(pinned), "pull_policy: never") || !strings.Contains(string(pinned), defaults) {
		t.Fatalf("pinned: %s %v", pinned, err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test runtime writes under t.TempDir
	if err != nil || strings.Count(string(log), "pull --quiet") != 1 || strings.Count(string(log), " up -d") != 2 {
		t.Fatalf("commands: %s %v", log, err)
	}
}

func TestEnvironmentFailureRedactsStartupAndCleanup(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	if err := os.Mkdir(filepath.Join(dir, "env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env", "defaults"), []byte("SECRET=private-marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(t.Context(), "env", "env.example", time.Second); err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(testManifest, "    image:", "    env_file: defaults\n    image:", 1)
	if err := db.SaveManifest(t.Context(), "env", ledger.Manifest{Content: []byte(source)}); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
 info) echo 'false true';;
 pull) echo sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;
 *) echo private-marker >&2; exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bin", "podman"), []byte(script), 0700); err != nil { // #nosec G306 -- fake Podman must be executable
		t.Fatal(err)
	}
	_, err := Launch(t.Context(), cfg, db, "env", nil)
	if err == nil || strings.Contains(err.Error(), "private-marker") || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("failure: %v", err)
	}
}

func TestFreshLaunchCommandsAndRestart(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	manifest := strings.Replace(testManifest, "    image: example/web:latest", "    image: example/web:latest\n    x-launcher: {launch_command: [echo, '$$VALUE', '']}", 1)
	if err := db.Create(t.Context(), "two", "two.example", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveManifest(t.Context(), "two", ledger.Manifest{Content: []byte(manifest)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "starting"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var activity []string
	ctx := WithLaunchProgress(t.Context(), func(message string) { activity = append(activity, message) })
	if _, err := Launch(ctx, cfg, db, "two", nil); err != nil {
		t.Fatal(err)
	}
	history := strings.Join(activity, "\n")
	for _, message := range []string{
		`Preparing the registry image for "web".`, "Starting containers with Podman Compose.",
		"Waiting for readiness", `Running a launch command for "web".`, "Checking readiness again after launch commands.",
	} {
		if !strings.Contains(history, message) {
			t.Errorf("missing activity %q in %s", message, history)
		}
	}
	if strings.Contains(history, "$VALUE") {
		t.Fatalf("launch command argument leaked into activity: %s", history)
	}
	if err := Stop(t.Context(), cfg, db, "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(t.Context(), cfg, db, "two"); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) // #nosec G304 -- test-owned temporary fixture
	if err != nil {
		t.Fatal(err)
	}
	before, after, _ := strings.Cut(string(log), "exec -- "+testImage+" echo $VALUE")
	if strings.Count(string(log), "exec -- ") != 1 || strings.Count(before, "container inspect") != 2 || !strings.Contains(after, "container inspect") {
		t.Fatalf("log: %s", log)
	}
	for _, message := range []string{
		"Pulling service image", "Waiting for container readiness", "Running fresh-launch command", "Fresh-launch command finished",
		"Checking readiness again", "Running Compose down", "Project cleanup verified", "Skipping fresh-launch commands for start",
	} {
		if !strings.Contains(logs.String(), `run_id="two" `+message) {
			t.Fatalf("missing message %q for run two: %s", message, logs.String())
		}
	}
	if strings.Contains(logs.String(), "$VALUE") {
		t.Fatalf("launch command argument leaked into diagnostics: %s", logs.String())
	}
}

func TestLaunchCommandFailureCleansUp(t *testing.T) {
	for _, failure := range []struct{ marker, message string }{
		{"failexec", "exit status 9"},
		{"exitafterexec", "container exited"},
	} {
		t.Run(failure.marker, func(t *testing.T) {
			cfg, db, dir := fakeRuntime(t)
			manifest := strings.Replace(testManifest, "    image: example/web:latest", "    image: example/web:latest\n    x-launcher: {launch_command: [echo]}", 1)
			if err := db.Create(t.Context(), "two", "two.example", time.Second); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveManifest(t.Context(), "two", ledger.Manifest{Content: []byte(manifest)}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, failure.marker), nil, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Launch(t.Context(), cfg, db, "two", nil)
			if err == nil || !strings.Contains(err.Error(), `service "web"`) || !strings.Contains(err.Error(), failure.message) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error: %v", err)
			}
			if _, err = os.Stat(filepath.Join(dir, "stopped")); err != nil {
				t.Fatal("cleanup did not run", err)
			}
		})
	}
}

func TestLaunchCommandTimeoutCleansUp(t *testing.T) {
	cfg, db, dir := fakeRuntime(t)
	manifest := strings.Replace(testManifest, "    image: example/web:latest", "    image: example/web:latest\n    x-launcher: {launch_command: [echo]}", 1)
	if err := db.Create(t.Context(), "two", "two.example", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveManifest(t.Context(), "two", ledger.Manifest{Content: []byte(manifest)}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sleepexec"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := Launch(ctx, cfg, db, "two", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), `service "web"`) {
		t.Fatalf("error: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "stopped")); err != nil {
		t.Fatal("cleanup did not run", err)
	}
}
