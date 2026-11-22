package manifest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

type localRunner struct {
	repo    string
	calls   int
	fetched string
}

type manifestWriteLostReply struct{ *ledger.Ledger }

func (l manifestWriteLostReply) SaveManifest(ctx context.Context, id string, manifest ledger.Manifest) error {
	if err := l.Ledger.SaveManifest(ctx, id, manifest); err != nil {
		return err
	}
	return errors.New("manifest write acknowledgement lost")
}

func (r *localRunner) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	r.calls++
	if len(args) > 0 && args[0] == "fetch" {
		r.fetched = args[len(args)-1]
	}
	for i, arg := range args {
		if strings.HasPrefix(arg, "ssh://git@github.com/") {
			args[i] = r.repo
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- controlled test fixture
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.New(string(out))
	}
	return out, nil
}
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...) // #nosec G204 -- controlled test fixture
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestAcquirePinnedCheckout(t *testing.T) {
	ctx := t.Context()
	const original = "services:\r\n  app: {}"
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	git(t, repo, "config", "core.autocrlf", "false")
	write(t, filepath.Join(repo, "deploy/compose.yaml"), original)
	write(t, filepath.Join(repo, "deploy/env"), "original\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "first")
	first := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "branch", "feature/slash")
	state := t.TempDir()
	db, err := ledger.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(ctx, "one", "one.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(ctx, "inline", "inline.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(ctx, "github", "github.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: state}
	runner := &localRunner{repo: repo}
	const baseURL = "https://github.com/team/repo/blob/feature/slash/deploy/"
	if _, err := acquire(ctx, cfg, db, "one", Input{Reference: baseURL + "missing.yaml"}, runner); err == nil {
		t.Fatal("missing manifest accepted")
	}
	assertCheckoutCount(t, state, 0)
	source := baseURL + "compose.yaml?plain=1#L1"
	got, err := acquire(ctx, cfg, db, "one", Input{Reference: source}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.fetched != first {
		t.Fatalf("fetch was not pinned: %s", runner.fetched)
	}
	if got.Commit != first || got.Path != "deploy/compose.yaml" || got.Repository != "team/repo" || string(got.Content) != original {
		t.Fatalf("unexpected manifest: %+v", got)
	}
	explicit := "github://team/repo/feature/slash/deploy/compose.yaml"
	gotExplicit, err := acquire(ctx, cfg, db, "github", Input{Reference: explicit}, runner)
	if err != nil || gotExplicit.Commit != got.Commit || gotExplicit.Path != got.Path || gotExplicit.Repository != got.Repository || string(gotExplicit.Content) != original {
		t.Fatalf("explicit GitHub reference: %+v %v", gotExplicit, err)
	}
	write(t, filepath.Join(repo, "deploy/env"), "changed\n")
	write(t, filepath.Join(repo, "deploy/compose.yaml"), "services:\n  replacement: {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "second")
	git(t, repo, "branch", "-f", "feature/slash", "HEAD")
	content, err := os.ReadFile(filepath.Join(state, "checkouts", got.Checkout, "deploy/env")) // #nosec G304 -- test fixture
	if err != nil || string(content) != "original\n" {
		t.Fatalf("relative file: %q %v", content, err)
	}
	if duplicate, err := acquire(ctx, cfg, db, "one", Input{Reference: source}, runner); err == nil || duplicate.Checkout != "" {
		t.Fatalf("duplicate save returned checkout %q, err %v", duplicate.Checkout, err)
	}
	for _, checkout := range []string{got.Checkout, gotExplicit.Checkout} {
		if _, err := os.Stat(filepath.Join(state, "checkouts", checkout, "deploy", "compose.yaml")); err != nil {
			t.Fatalf("retained checkout %q: %v", checkout, err)
		}
	}
	inline, err := Acquire(ctx, cfg, db, "inline", Input{InlineYAML: []byte(original)})
	if err != nil || string(inline.Content) != original || inline.Checkout != "" {
		t.Fatalf("inline: %+v %v", inline, err)
	}
	assertCheckoutCount(t, state, 2)
	if err := db.Create(ctx, "lost-reply", "lost-reply.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(ctx, cfg, manifestWriteLostReply{db}, "lost-reply", Input{Reference: source}, runner); err == nil {
		t.Fatal("expected a manifest write error")
	}
	assertCheckoutCount(t, state, 3)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = ledger.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	saved, err := db.GetManifest(ctx, "one")
	if err != nil || saved.Commit != first || saved.Repository != "team/repo" || saved.Path != "deploy/compose.yaml" || string(saved.Content) != original || saved.Checkout != got.Checkout {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	savedExplicit, err := db.GetManifest(ctx, "github")
	if err != nil || savedExplicit.Commit != first || savedExplicit.Repository != saved.Repository || savedExplicit.Path != saved.Path || string(savedExplicit.Content) != original || savedExplicit.Checkout != gotExplicit.Checkout {
		t.Fatalf("saved explicit GitHub reference: %+v %v", savedExplicit, err)
	}
	inline, err = db.GetManifest(ctx, "inline")
	if err != nil || string(inline.Content) != string(saved.Content) || inline.Repository != "" || inline.Commit != "" || inline.Path != "" || inline.Checkout != "" {
		t.Fatalf("saved inline: %+v %v", inline, err)
	}
}

func assertCheckoutCount(t *testing.T, state string, count int) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "checkouts"))
	if err != nil || len(entries) != count {
		t.Fatalf("checkouts: %v, %v; want %d entries", entries, err, count)
	}
}

func TestValidationBeforeTransport(t *testing.T) {
	ctx := context.Background()
	db, err := ledger.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Create(ctx, "x", "x.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	runner := &localRunner{}
	cfg := config.Config{}
	for _, raw := range []string{"https://github.com/team/repo/blob", "https://github.com/team/repo/blob/main/../file", "github://team/repo/main", "https://github.com/team/repo/blob/main%2Fdir/file"} {
		if _, err := acquire(ctx, cfg, db, "x", Input{Reference: raw}, runner); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if runner.calls != 0 {
		t.Fatal("unexpected transport")
	}
}

type scriptedRunner struct {
	output []byte
	err    error
	args   [][]string
}

func (r *scriptedRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.args = append(r.args, args)
	return r.output, r.err
}
func TestResolution(t *testing.T) {
	ctx := context.Background()
	oid := strings.Repeat("a", 40)
	r := &scriptedRunner{output: []byte(oid + "\trefs/heads/feature\n" + oid + "\trefs/heads/feature/slash\n")}
	if _, _, err := resolve(ctx, r, "remote", []string{"feature", "slash", "file"}); err == nil {
		t.Fatal("ambiguous ref accepted")
	}
	r.output = []byte(oid + "\trefs/heads/feature/slash\n")
	got, file, err := resolve(ctx, r, "remote", []string{"feature", "slash", "file"})
	if err != nil || got != oid || file != "file" {
		t.Fatalf("resolve: %s %s %v", got, file, err)
	}
	r.output = append(r.output, []byte(strings.Repeat("b", 40)+"\trefs/tags/feature/slash\n")...)
	if _, _, err := resolve(ctx, r, "remote", []string{"feature", "slash", "file"}); err == nil {
		t.Fatal("conflicting branch and tag accepted")
	}
	r.output = []byte(oid + "\trefs/heads/refs/tags/release\u00a0candidate\n")
	got, file, err = resolve(ctx, r, "remote", []string{"refs", "tags", "release\u00a0candidate", "file"})
	if err != nil || got != oid || file != "file" {
		t.Fatalf("nested ref: %s %s %v", got, file, err)
	}
	got, file, err = resolve(ctx, r, "remote", []string{oid, "file"})
	if err != nil || got != oid || file != "file" {
		t.Fatalf("commit: %s %s %v", got, file, err)
	}
	r.err = errors.New("transport failed")
	if _, _, err := resolve(ctx, r, "remote", []string{"main", "file"}); err == nil {
		t.Fatal("transport failure ignored")
	}
}
func TestGitRunnerIsolation(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "invalid")
	t.Setenv("GIT_CONFIG_VALUE_0", "invalid")
	r := gitRunner{stateDir: t.TempDir()}
	if _, err := r.run(context.Background(), t.TempDir(), "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(context.Background(), t.TempDir(), "not-a-command"); err == nil {
		t.Fatal("expected git failure")
	}
}

func TestCheckoutFailures(t *testing.T) {
	ctx := t.Context()
	r := &scriptedRunner{err: errors.New("failure")}
	if _, err := checkoutManifest(ctx, r, "remote", "oid", "file", t.TempDir(), "team/repo"); err == nil {
		t.Fatal("expected git failure")
	}
	state := t.TempDir()
	db, err := ledger.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Create(ctx, "one", "one.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(state, "checkouts")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: state}
	const base = "https://github.com/team/repo/blob/"
	r.err = nil
	for _, tc := range []struct{ id, ref, message string }{
		{"missing", "main", "no rows"},
		{"one", "main", "ref is missing"},
		{"one", strings.Repeat("a", 40), "mode 0700 directory"},
	} {
		if _, err := acquire(ctx, cfg, db, tc.id, Input{Reference: base + tc.ref + "/compose.yaml"}, r); err == nil || !strings.Contains(err.Error(), tc.message) {
			t.Errorf("acquire with id %q, ref %q returned %v, want %q", tc.id, tc.ref, err, tc.message)
		}
	}
}

func TestGitRunnerIgnoresWorkingDirectoryConfig(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "url.ssh://attacker/.insteadOf", "ssh://git@github.com/")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(original) }()
	r := gitRunner{stateDir: t.TempDir()}
	if _, err := r.run(context.Background(), "", "config", "--get", "url.ssh://attacker/.insteadOf"); err == nil {
		t.Fatal("read ambient repository config")
	}
}
func TestInvalidBlob(t *testing.T) {
	r := &scriptedRunner{output: []byte("120000 blob abc\tfile\x00")}
	if err := checkBlob(context.Background(), r, "", "file"); err == nil {
		t.Fatal("accepted symlink")
	}
	r.output = []byte("100644 blob abc\tother\x00")
	if err := checkBlob(context.Background(), r, "", "file"); err == nil {
		t.Fatal("accepted wrong path")
	}
	r.err = errors.New("git failed")
	if err := checkBlob(context.Background(), r, "", "file"); err == nil {
		t.Fatal("ignored git failure")
	}
}

func TestAcquireUsesAccountSSH(t *testing.T) {
	ctx := t.Context()
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "compose.yaml"), "services: {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "first")
	oid := git(t, repo, "rev-parse", "HEAD")
	home := t.TempDir()
	write(t, filepath.Join(home, ".gitconfig"), "[url \"ssh://attacker/\"]\n insteadOf = ssh://git@github.com/\n")
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "-G" ]; then exit 0; fi
[ "$1" = "-o" ] && [ "$2" = "BatchMode=yes" ] || exit 91
shift 2
if [ "$1" = "-o" ] && [ "$2" = "SendEnv=GIT_PROTOCOL" ]; then shift 2; fi
[ "$1" = "git@github.com" ] && [ "$2" = "git-upload-pack '/team/repo.git'" ] || exit 91
[ "$HOME" = "$FIXTURE_HOME" ] && [ "$SSH_AUTH_SOCK" = "$FIXTURE_SOCK" ] || exit 92
[ ! -f "$FIXTURE_DENY" ] || exit 93
exec git-upload-pack "$FIXTURE_REPO"
`
	ssh := filepath.Join(bin, "ssh")
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil { // #nosec G306 -- the SSH fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(home, "agent.sock"))
	t.Setenv("FIXTURE_HOME", home)
	t.Setenv("FIXTURE_SOCK", filepath.Join(home, "agent.sock"))
	t.Setenv("FIXTURE_REPO", repo)
	deny := filepath.Join(home, "deny")
	t.Setenv("FIXTURE_DENY", deny)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.ssh://attacker/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "ssh://git@github.com/")
	state := t.TempDir()
	db, err := ledger.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Create(ctx, "one", "one.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: state}
	source := "https://github.com/team/repo/blob/" + oid + "/compose.yaml"
	write(t, deny, "deny")
	if _, err := Acquire(ctx, cfg, db, "one", Input{Reference: source}); err == nil {
		t.Fatal("expected authentication failure")
	}
	assertCheckoutCount(t, state, 0)
	if _, err := db.GetManifest(ctx, "one"); err == nil {
		t.Fatal("manifest saved after failure")
	}
	if _, err := db.Get(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deny); err != nil {
		t.Fatal(err)
	}
	got, err := Acquire(ctx, cfg, db, "one", Input{Reference: source})
	if err != nil {
		t.Fatal(err)
	}
	if got.Commit != oid || string(got.Content) != "services: {}\n" {
		t.Fatalf("unexpected manifest: %+v", got)
	}
}

func TestAcquireInline(t *testing.T) {
	ctx := t.Context()
	state := t.TempDir()
	db, err := ledger.Open(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Create(ctx, "inline", "inline.example", time.Hour); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: state}
	runner := &localRunner{}
	for _, input := range []Input{{}, {Reference: "https://github.com/a/b/blob/main/file", InlineYAML: []byte{}}} {
		if _, err := acquire(ctx, cfg, db, "inline", input, runner); err == nil || !strings.Contains(err.Error(), "exactly one manifest reference or inline Compose YAML") {
			t.Fatalf("invalid sources: %v", err)
		}
	}
	if _, err := db.GetManifest(ctx, "inline"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected no saved manifest: %v", err)
	}
	got, err := acquire(ctx, cfg, db, "inline", Input{InlineYAML: []byte{}}, runner)
	if err != nil || got.Content == nil || len(got.Content) != 0 {
		t.Fatalf("acquire: %+v %v", got, err)
	}
	duplicate, err := acquire(ctx, cfg, db, "inline", Input{InlineYAML: []byte("changed")}, runner)
	if err == nil || duplicate.Content != nil || duplicate.Repository != "" || duplicate.Commit != "" || duplicate.Path != "" || duplicate.Checkout != "" {
		t.Fatalf("duplicate: %+v %v", duplicate, err)
	}
	saved, err := db.GetManifest(ctx, "inline")
	if err != nil || saved.Content == nil || len(saved.Content) != 0 {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	if runner.calls != 0 {
		t.Fatal("unexpected Git invocation")
	}
	if _, err := os.Stat(filepath.Join(state, "checkouts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected checkout: %v", err)
	}
}
