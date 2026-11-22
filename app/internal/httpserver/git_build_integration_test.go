package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/podman"
)

func TestGitBuildFailureStatus(t *testing.T) {
	if os.Getenv("STACK_LAUNCHER_PODMAN_TEST") != "1" {
		t.Skip("set STACK_LAUNCHER_PODMAN_TEST=1 to run real rootless Podman integration")
	}
	a, router := setupAPI(t)
	a.launch = podman.Launch
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	installBrokenBuildFixture(t, ctx)
	manifest := `services:
  web:
    build: .
    x-launcher:
      git: {repository: team/repo, ref: fixture}
x-launcher:
  endpoint: {service: web, port: 8080, https: false}
`
	body, err := json.Marshal(map[string]string{"inline_yaml": manifest})
	if err != nil {
		t.Fatal(err)
	}
	id := postID(t, a, string(body))
	status := pollFailedBuild(t, ctx, a, id)
	if status.FailurePhase != "startup" || status.URL != "" || !strings.Contains(status.Failure, `"web"`) || !strings.Contains(status.Failure, "podman build") || !strings.Contains(strings.ToLower(status.Failure), "brokeninstruction") {
		t.Fatalf("stack status: %+v", status)
	}
	if p, n := router.counts(); p != 0 || n != 0 {
		t.Fatalf("route operations: publish=%d abort=%d", p, n)
	}
	if _, err := a.db.GetImages(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected no saved images: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(a.cfg.StateDir, "checkouts"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary checkouts left behind: %v (read error %v)", entries, err)
	}
	project := compose.ProjectName(id)
	if output := runBuildFixtureCommand(t, ctx, "", "podman", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project); output != "" {
		t.Fatalf("project containers after failure: %s", output)
	}
}

func installBrokenBuildFixture(t *testing.T, ctx context.Context) {
	t.Helper()
	repo, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM scratch\nBROKENINSTRUCTION fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runBuildFixtureCommand(t, ctx, repo, "git", "init", "-qb", "fixture")
	runBuildFixtureCommand(t, ctx, repo, "git", "add", "Dockerfile")
	runBuildFixtureCommand(t, ctx, repo, "git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "broken Dockerfile")
	const ssh = `#!/bin/sh
if [ "$1" = "-G" ]; then exit 0; fi
[ "$1" = "-o" ] && [ "$2" = "BatchMode=yes" ] || exit 91
shift 2
if [ "$1" = "-o" ] && [ "$2" = "SendEnv=GIT_PROTOCOL" ]; then shift 2; fi
[ "$1" = "git@github.com" ] && [ "$2" = "git-upload-pack '/team/repo.git'" ] || exit 92
exec git-upload-pack "$BUILD_FIXTURE_REPO"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0700); err != nil { // #nosec G306 -- test-owned SSH fixture
		t.Fatal(err)
	}
	// The isolated launcher Git runner filters variables beginning with GIT_.
	t.Setenv("BUILD_FIXTURE_REPO", repo)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func runBuildFixtureCommand(t *testing.T, ctx context.Context, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- test fixture commands and arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func pollFailedBuild(t *testing.T, ctx context.Context, a *api, id string) ledger.Status {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		w := serve(a, http.MethodGet, "/stacks/"+id, "")
		var status ledger.Status
		if err := json.Unmarshal(w.Body.Bytes(), &status); w.Code != http.StatusOK || err != nil {
			t.Fatalf("status HTTP %d: %s (%v)", w.Code, w.Body, err)
		}
		if status.State == "failed" {
			return status
		}
		if status.State != "launching" {
			t.Fatalf("expected launching or failed: %+v", status)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for stack %s: %v", id, ctx.Err())
		case <-ticker.C:
		}
	}
}
