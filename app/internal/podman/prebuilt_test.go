package podman

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/registry"
)

const prebuiltRepo = "registry.example.com/team/web"
const prebuiltDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const prebuiltStack = "prebuilt"
const originalDockerfile = "FROM scratch\n"

type prebuiltFixture struct {
	cfg    config.Config
	db     *ledger.Ledger
	repo   string
	commit string
}

func setupPrebuiltFixture(t *testing.T, ref string) prebuiltFixture {
	t.Helper()
	cfg, db, dir := fakeRuntime(t)
	f := prebuiltFixture{cfg: cfg, db: db, repo: t.TempDir()}
	f.git(t, "init", "-q", "-b", "main")
	f.writeDockerfile(t, originalDockerfile)
	f.git(t, "add", "Dockerfile")
	f.git(t, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "first")
	f.commit = f.git(t, "rev-parse", "HEAD")
	const ssh = `#!/bin/sh
if [ "$1" = "-G" ]; then exit 0; fi
if [ -f "$FAKE_DIR/failssh" ]; then exit 1; fi
while [ "$1" = "-o" ]; do shift 2; done
[ "$1" = "git@github.com" ] && [ "$2" = "git-upload-pack '/team/web.git'" ] || exit 2
exec git-upload-pack "$PREBUILT_TEST_REPOSITORY"
`
	if err := os.WriteFile(filepath.Join(dir, "bin", "ssh"), []byte(ssh), 0700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}
	t.Setenv("PREBUILT_TEST_REPOSITORY", f.repo)
	if err := db.Create(t.Context(), prebuiltStack, "prebuilt.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`services:
  web:
    image: ignored.example/team/web:latest
    build: .
    x-launcher:
      git: {repository: team/web, ref: %q, prebuilt_repository: %q}
x-launcher:
  endpoint: {service: web, port: 8080, https: false}
`, ref, prebuiltRepo)
	if err := db.SaveManifest(t.Context(), prebuiltStack, ledger.Manifest{Content: []byte(manifest)}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f prebuiltFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) // #nosec G204 -- fixed test fixture arguments
	cmd.Dir = f.repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f prebuiltFixture) writeDockerfile(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, "Dockerfile"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f prebuiltFixture) assertLookup(t *testing.T, repository, tag, auth string) {
	t.Helper()
	if repository != prebuiltRepo || tag != "git-"+f.commit || auth != f.cfg.RegistryAuthFile {
		t.Fatalf("unexpected prebuilt lookup: %q %q %q", repository, tag, auth)
	}
}

func (f prebuiltFixture) file(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.cfg.StateDir, name)) // #nosec G304 -- test runtime writes under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLaunchGitPrebuiltPullsDigestAndStartReusesIt(t *testing.T) {
	f := setupPrebuiltFixture(t, "main")
	ctx := t.Context()
	got, err := launch(ctx, f.cfg, f.db, prebuiltStack, nil, func(_ context.Context, repository, tag, auth string) (string, error) {
		f.assertLookup(t, repository, tag, auth)
		return prebuiltDigest, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := prebuiltRepo + "@" + prebuiltDigest
	want := ledger.Image{ID: testImage, Repository: "team/web", Commit: f.commit, Reference: ref}
	saved, err := f.db.GetImages(ctx, prebuiltStack)
	if err != nil || saved["web"] != want {
		t.Fatalf("saved image: %+v %v", saved, err)
	}
	for _, name := range []string{"failssh", "failpull"} {
		if err := os.WriteFile(filepath.Join(f.cfg.StateDir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Stop(ctx, f.cfg, f.db, prebuiltStack); err != nil {
		t.Fatal(err)
	}
	restarted, err := Start(ctx, f.cfg, f.db, prebuiltStack)
	if err != nil || restarted.ProjectName != got.ProjectName || restarted.Endpoint != got.Endpoint {
		t.Fatalf("restart: %+v %v", restarted, err)
	}
	log := f.file(t, "log")
	pull := "pull --quiet --authfile " + f.cfg.RegistryAuthFile + " -- docker://" + ref
	if strings.Count(log, "pull --quiet") != 1 || !strings.Contains(log, pull) || strings.Contains(log, "build --layers") {
		t.Fatalf("commands: %s", log)
	}
	pinned := f.file(t, "pinned")
	if !strings.Contains(pinned, "sha256:"+testImage) || !strings.Contains(pinned, "pull_policy: never") || strings.Contains(pinned, prebuiltRepo) {
		t.Fatalf("pinned Compose: %s", pinned)
	}
}

func TestLaunchGitPrebuiltMissBuildsResolvedCommit(t *testing.T) {
	f := setupPrebuiltFixture(t, "main")
	ctx := t.Context()
	_, err := launch(ctx, f.cfg, f.db, prebuiltStack, nil, func(_ context.Context, repository, tag, auth string) (string, error) {
		f.assertLookup(t, repository, tag, auth)
		f.writeDockerfile(t, "FROM scratch\n# Second commit\n")
		f.git(t, "add", "Dockerfile")
		f.git(t, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "second")
		return "", fmt.Errorf("manifest absent: %w", registry.ErrNotFound)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := ledger.Image{ID: testImage, Repository: "team/web", Commit: f.commit}
	saved, err := f.db.GetImages(ctx, prebuiltStack)
	if err != nil || saved["web"] != want {
		t.Fatalf("saved image: %+v %v", saved, err)
	}
	if dockerfile := f.file(t, "built-dockerfile"); dockerfile != originalDockerfile {
		t.Fatalf("unexpected Dockerfile: %q", dockerfile)
	}
	if log := f.file(t, "log"); strings.Count(log, "build --layers") != 1 || strings.Contains(log, "pull --quiet") {
		t.Fatalf("commands: %s", log)
	}
}

func TestLaunchGitPrebuiltReportsSelectionFailures(t *testing.T) {
	for _, tt := range []struct {
		name      string
		ref       string
		lookupErr error
		marker    string
		wantError string
	}{
		{name: "Git ref missing", ref: "missing", wantError: "resolve Git ref"},
		{name: "registry denied", ref: "main", lookupErr: errors.New("registry denied"), wantError: "lookup prebuilt image"},
		{name: "digest pull failed", ref: "main", marker: "failpull", wantError: "pull image"},
		{name: "fallback build failed", ref: "main", lookupErr: registry.ErrNotFound, marker: "failbuild", wantError: "build image"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setupPrebuiltFixture(t, tt.ref)
			if tt.marker != "" {
				if err := os.WriteFile(filepath.Join(f.cfg.StateDir, tt.marker), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			_, err := launch(ctx, f.cfg, f.db, prebuiltStack, nil, func(_ context.Context, repository, tag, auth string) (string, error) {
				f.assertLookup(t, repository, tag, auth)
				return prebuiltDigest, tt.lookupErr
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) || !strings.Contains(err.Error(), `service "web"`) {
				t.Fatalf("launch error: %v", err)
			}
			if _, err := f.db.GetImages(ctx, prebuiltStack); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("unexpected saved images: %v", err)
			}
			log := f.file(t, "log")
			if tt.marker != "failbuild" && strings.Contains(log, "build --layers") {
				t.Fatalf("unexpected fallback build: %s", log)
			}
			if tt.marker != "failpull" && strings.Contains(log, "pull --quiet") {
				t.Fatalf("unexpected pull: %s", log)
			}
		})
	}
}

func TestPrebuiltFallbackRejectsDifferentCheckoutCommit(t *testing.T) {
	f := setupPrebuiltFixture(t, "main")
	f.git(t, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "tag", "-a", "release", "-m", "release")
	tagObject := f.git(t, "rev-parse", "refs/tags/release")
	build := &compose.GitBuild{Repository: "team/web", Ref: "main"}
	dir, _, err := checkoutImageSource(t.Context(), f.cfg.StateDir, build, tagObject)
	if dir != "" || err == nil || !strings.Contains(err.Error(), f.commit) || !strings.Contains(err.Error(), tagObject) {
		t.Fatalf("checkout: %q %v", dir, err)
	}
	checkouts, err := os.ReadDir(filepath.Join(f.cfg.StateDir, "checkouts"))
	if err != nil || len(checkouts) != 0 {
		t.Fatalf("checkouts: %v %v", checkouts, err)
	}
}
