package podman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

func TestGitBuildRestartIntegration(t *testing.T) {
	if os.Getenv("STACK_LAUNCHER_PODMAN_TEST") != "1" {
		t.Skip("set STACK_LAUNCHER_PODMAN_TEST=1 for real Podman")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repo := t.TempDir()
	bin := t.TempDir()
	state := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed test fixture arguments
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(repo, "app", "docker"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("app/message", "from checkout\n")
	write("app/docker/Containerfile", `FROM docker.io/library/nginx:1-alpine AS runtime
ARG MESSAGE
COPY message /tmp/message
RUN printf '%s %s\n' "$MESSAGE" "$(cat /tmp/message)" > /usr/share/nginx/html/index.html
FROM scratch AS unused
`)
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "first")
	firstCommit := git("rev-parse", "HEAD")
	sshAttempt := filepath.Join(bin, "unexpected-ssh")
	ssh := `#!/bin/sh
if [ "$1" = "-G" ]; then exit 0; fi
if [ -n "$BUILD_TEST_DENY_SSH" ]; then
  touch "$BUILD_TEST_DENY_SSH"
  exit 93
fi
while [ "$1" = "-o" ]; do shift 2; done
[ "$1" = "git@github.com" ] && [ "$2" = "git-upload-pack '/team/repo.git'" ] || exit 92
exec git-upload-pack "$BUILD_TEST_REPO"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILD_TEST_REPO", repo)
	t.Setenv("BUILD_TEST_DENY_SSH", "")
	db, err := ledger.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	id := fmt.Sprintf("git-build-integration-%d", time.Now().UnixNano())
	if err = db.Create(ctx, id, "git.example", time.Second); err != nil {
		t.Fatal(err)
	}
	source := `services:
  web:
    build: {context: app, dockerfile: docker/Containerfile, target: runtime, args: [MESSAGE=first]}
    x-launcher: {git: {repository: team/repo, ref: main}}
  helper:
    image: docker.io/library/nginx:1-alpine
x-launcher: {endpoint: {service: web, port: 80, https: false}}
`
	if err = db.SaveManifest(ctx, id, ledger.Manifest{Content: []byte(source)}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: state}
	defer func() { _ = Stop(context.Background(), cfg, db, id) }()
	result, err := Launch(ctx, cfg, db, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetImages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if saved["web"].Commit != firstCommit || saved["web"].Repository != "team/repo" || saved["helper"].Reference != "docker.io/library/nginx:1-alpine" {
		t.Fatalf("image provenance: %+v", saved)
	}
	assertImages := func(result Result) {
		t.Helper()
		r := runner{env: cleanEnv("")}
		for service, image := range saved {
			actual, err := r.run(ctx, "container", "inspect", "--format", "{{.Image}}", result.containerIDs[service])
			if err != nil || strings.TrimPrefix(actual, "sha256:") != image.ID {
				t.Fatalf("service %s is using image %q, want %q (%v)", service, actual, image.ID, err)
			}
		}
	}
	assertContent := func(address string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK || strings.TrimSpace(string(data)) != "first from checkout" {
			t.Fatalf("HTTP %d, content %q: %v", resp.StatusCode, data, err)
		}
	}
	assertImages(result)
	assertContent(result.Endpoint.String())
	write("app/docker/Containerfile", "FROM scratch\n")
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "second")
	if git("rev-parse", "main") == firstCommit {
		t.Fatal("build ref did not move")
	}
	t.Setenv("BUILD_TEST_DENY_SSH", sshAttempt)
	if err = Stop(ctx, cfg, db, id); err != nil {
		t.Fatal(err)
	}
	result, err = Start(ctx, cfg, db, id)
	if err != nil {
		t.Fatal(err)
	}
	assertImages(result)
	assertContent(result.Endpoint.String())
	if _, err := os.Stat(sshAttempt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stop or Start tried fetching build sources: %v", err)
	}
	after, err := db.GetImages(ctx, id)
	if err != nil || after["web"] != saved["web"] {
		t.Fatalf("saved image changed: %+v %v", after, err)
	}
}
