package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutBuild(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-qb", "feature/slash")
	git(t, repo, "config", "user.email", "test@example.invalid")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "app/Dockerfile"), "FROM scratch\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "source")
	commit := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "tag", "-am", "build tag", "release/slash")
	installBuildSSH(t, repo)
	state := t.TempDir()
	for _, ref := range []string{"feature/slash", "release/slash", strings.ToUpper(commit)} {
		t.Run(ref, func(t *testing.T) {
			dir, got, err := CheckoutBuild(t.Context(), state, "Team/Repo", ref)
			if err != nil || got != commit {
				t.Fatalf("checkout returned commit %q, want %q (%v)", got, commit, err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			if filepath.Dir(dir) != filepath.Join(state, "checkouts") {
				t.Fatalf("build source outside the checkout directory: %q", dir)
			}
			content, err := os.ReadFile(filepath.Join(dir, "app/Dockerfile")) // #nosec G304 -- test checkout
			if err != nil || string(content) != "FROM scratch\n" {
				t.Fatalf("checked-out Dockerfile: %q (%v)", content, err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Git metadata in build source: %v", err)
			}
		})
	}
	assertCheckoutCount(t, state, 0)
}

func TestResolveBuildCommitPinsAdvertisedRevision(t *testing.T) {
	repo, state := t.TempDir(), t.TempDir()
	git(t, repo, "init", "-qb", "feature/slash")
	git(t, repo, "config", "user.email", "test@example.invalid")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "Dockerfile"), "FROM scratch\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "source")
	firstCommit := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "branch", "review/slash")
	git(t, repo, "tag", "-am", "build tag", "release/annotated")
	git(t, repo, "tag", "release/lightweight")
	git(t, repo, "tag", "-am", "tag shadows branch", "feature/slash")
	installBuildSSH(t, repo)

	for _, ref := range []string{"review/slash", "release/annotated", "release/lightweight"} {
		if commit, err := ResolveBuildCommit(t.Context(), state, "Team/Repo", ref); err != nil || commit != firstCommit {
			t.Errorf("resolve %q returned %q, want %q (%v)", ref, commit, firstCommit, err)
		}
	}
	for _, ref := range []string{"feature/slash", "release/annotated^{}", "missing"} {
		if commit, err := ResolveBuildCommit(t.Context(), state, "Team/Repo", ref); err == nil || !strings.Contains(err.Error(), "missing or ambiguous") || commit != "" {
			t.Errorf("resolve %q returned %q and %v, want missing or ambiguous error", ref, commit, err)
		}
	}
	if _, err := ResolveBuildCommit(t.Context(), state, "Team/..", firstCommit); err == nil || !strings.Contains(err.Error(), "invalid repository or ref") {
		t.Fatalf("resolve invalid source: %v", err)
	}
	t.Setenv("DENY_BUILD_SSH", "1")
	if commit, err := ResolveBuildCommit(t.Context(), state, "Team/Repo", strings.ToUpper(firstCommit)); err != nil || commit != firstCommit {
		t.Fatalf("resolve pinned SHA without SSH returned %q, want %q (%v)", commit, firstCommit, err)
	}
	t.Setenv("DENY_BUILD_SSH", "0")
	pinned, err := ResolveBuildCommit(t.Context(), state, "Team/Repo", "release/annotated")
	if err != nil {
		t.Fatal(err)
	}
	assertCheckoutCount(t, state, 0)
	git(t, repo, "commit", "--allow-empty", "-qm", "updated source")
	git(t, repo, "tag", "-fam", "moved build tag", "release/annotated")
	updated := git(t, repo, "rev-parse", "HEAD")
	if commit, err := ResolveBuildCommit(t.Context(), state, "Team/Repo", "release/annotated"); err != nil || commit != updated {
		t.Fatalf("resolve moved tag returned %q, want %q (%v)", commit, updated, err)
	}
	dir, commit, err := CheckoutBuild(t.Context(), state, "Team/Repo", pinned)
	if err != nil || commit != firstCommit {
		t.Fatalf("checkout selected commit returned %q, want %q (%v)", commit, firstCommit, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
}

func TestCheckoutBuildFailures(t *testing.T) {
	state, repo := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "checkouts"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-qb", "main")
	installBuildSSH(t, repo)
	unknown := strings.Repeat("a", 40)
	for _, tc := range []struct{ repository, ref, message string }{
		{"team/repo;unexpected", "main", "invalid repository or ref"},
		{"team/..", "main", "invalid repository or ref"},
		{"team/repo", "", "invalid repository or ref"},
		{"team/repo", "main\nwrong", "invalid repository or ref"},
		{"team/repo", "missing", "ref is missing or ambiguous"},
		{"team/repo", unknown, "git fetch"},
	} {
		if dir, commit, err := CheckoutBuild(t.Context(), state, tc.repository, tc.ref); err == nil || !strings.Contains(err.Error(), tc.message) || dir != "" || commit != "" {
			t.Errorf("checkout %q at %q returned directory %q, commit %q, error %v, want %q", tc.repository, tc.ref, dir, commit, err, tc.message)
		}
		assertCheckoutCount(t, state, 0)
	}
	if _, _, err := CheckoutBuild(t.Context(), filepath.Join(state, "absent"), "team/repo", unknown); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkout in missing state directory: %v", err)
	}
	t.Setenv("DENY_BUILD_SSH", "1")
	if _, _, err := CheckoutBuild(t.Context(), state, "team/repo", "main"); err == nil || !strings.Contains(err.Error(), "git ls-remote") {
		t.Fatalf("denied build SSH: %v", err)
	}
	assertCheckoutCount(t, state, 0)
}

func installBuildSSH(t *testing.T, repo string) {
	t.Helper()
	bin := t.TempDir()
	const script = `#!/bin/sh
if [ "$1" = "-G" ]; then exit 0; fi
[ "$1" = "-o" ] && [ "$2" = "BatchMode=yes" ] || exit 91
shift 2
if [ "$1" = "-o" ] && [ "$2" = "SendEnv=GIT_PROTOCOL" ]; then shift 2; fi
[ "$1" = "git@github.com" ] && [ "$2" = "git-upload-pack '/team/repo.git'" ] || exit 92
[ "$DENY_BUILD_SSH" = "0" ] || exit 93
exec git-upload-pack "$BUILD_SOURCE_REPO"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0700); err != nil { // #nosec G306 -- test-owned SSH fixture
		t.Fatal(err)
	}
	t.Setenv("BUILD_SOURCE_REPO", repo)
	t.Setenv("DENY_BUILD_SSH", "0")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
