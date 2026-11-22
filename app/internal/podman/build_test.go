package podman

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"stack-launcher.local/app/internal/compose"
	"strings"
	"testing"
)

func TestBuildPathConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildPath(root, root, ".", true); err != nil {
		t.Fatal(err)
	}
	if _, err := buildPath(root, root, "Dockerfile", false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"escape", "..", outside} {
		if _, err := buildPath(root, root, path, true); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	if _, err := buildPath(root, root, "Dockerfile", true); err == nil {
		t.Fatal("accepted file as directory")
	}
}

func TestBuildHelpers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	b := &compose.GitBuild{Context: ".", Dockerfile: "Dockerfile", Target: "release", Args: []string{"A=B"}}
	contextPath, file, err := buildPaths(root, b)
	if err != nil {
		t.Fatal(err)
	}
	args := buildArguments("iid", file, contextPath, "auth", b)
	if !slices.Contains(args, "--authfile") || !slices.Contains(args, "--target") || !slices.Contains(args, "A=B") {
		t.Fatalf("args: %v", args)
	}
	b.Dockerfile = "missing"
	if _, _, err = buildPaths(root, b); err == nil {
		t.Fatal("missing Dockerfile")
	}
	b.Context = "missing"
	if _, _, err = buildPaths(root, b); err == nil {
		t.Fatal("missing context")
	}
	iid := filepath.Join(root, "iid")
	if _, err = readImageID(iid); err == nil {
		t.Fatal("missing iid")
	}
	if err = os.WriteFile(iid, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readImageID(iid); err == nil {
		t.Fatal("invalid iid")
	}
	expected := strings.Repeat("a", 64)
	if err = os.WriteFile(iid, []byte("sha256:"+expected), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := readImageID(iid); err != nil || got != expected {
		t.Fatalf("iid: %s %v", got, err)
	}
}
func TestLimitedWriter(t *testing.T) {
	var b bytes.Buffer
	w := limitedWriter{writer: &b, remaining: 3}
	if n, err := w.Write([]byte("hello")); err != nil || n != 5 || b.String() != "hel" {
		t.Fatalf("write: %d %v %q", n, err, b.String())
	}
	_, _ = w.Write([]byte("again"))
}

func TestBuildImageWithGitAndPodman(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	bin := t.TempDir()
	state := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed test fixture arguments
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte("FROM scratch"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "first")
	ssh := "#!/bin/sh\nwhile [ \"$1\" = \"-o\" ]; do shift 2; done\nexec git-upload-pack \"$TEST_REPOSITORY\"\n"
	podman := "#!/bin/sh\nwhile [ \"$1\" != \"--iidfile\" ]; do shift; done\nprintf '%064d' 0 > \"$2\"\n"
	for name, script := range map[string]string{"ssh": ssh, "podman": podman} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	} // #nosec G306 -- executable test fixtures
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_REPOSITORY", repo)
	b := &compose.GitBuild{Repository: "team/repo", Ref: "main", Context: ".", Dockerfile: "Dockerfile"}
	image, err := buildImage(ctx, runner{env: os.Environ()}, state, "", b, "")
	if err != nil || image.Repository != "team/repo" || image.Commit == "" || len(image.ID) != 64 {
		t.Fatalf("image: %+v %v", image, err)
	}
	entries, err := os.ReadDir(filepath.Join(state, "checkouts"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("checkouts: %v %v", entries, err)
	}
	b.Ref = "missing"
	if _, err := buildImage(ctx, runner{env: os.Environ()}, state, "", b, ""); err == nil {
		t.Fatal("missing ref accepted")
	}
	b.Ref = "main"
	b.Context = "missing"
	if _, err := buildImage(ctx, runner{env: os.Environ()}, state, "", b, ""); err == nil {
		t.Fatal("missing context accepted")
	}
}
