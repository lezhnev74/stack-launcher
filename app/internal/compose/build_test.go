package compose

import (
	"strings"
	"testing"

	"stack-launcher.local/app/internal/ledger"
)

func TestGitBuildPreparation(t *testing.T) {
	source := `services:
  web:
    image: ignored:latest
    build:
      context: src
      dockerfile: custom/Dockerfile
      target: final
      args: [VALUE=$$HOME]
    x-launcher:
      git: {repository: owner/repo, ref: main, prebuilt_repository: registry.example.test:5000/team/web}
      launch_command: [echo, ok]
x-launcher: {endpoint: {service: web, port: 80}}
`
	p, err := Prepare("/tmp", "id", ledger.Manifest{Content: []byte(source)})
	if err != nil {
		t.Fatal(err)
	}
	b := p.Services["web"].Build
	if b == nil || b.Context != "src" || b.Dockerfile != "custom/Dockerfile" || b.Target != "final" || len(b.Args) != 1 || b.Args[0] != "VALUE=$HOME" {
		t.Fatalf("build: %#v", b)
	}
	if b.PrebuiltRepository != "registry.example.test:5000/team/web" {
		t.Fatalf("prebuilt repository = %q", b.PrebuiltRepository)
	}
	if strings.Contains(string(p.YAML), "build:") || strings.Contains(string(p.YAML), "x-launcher") || len(p.LaunchCommands) != 1 {
		t.Fatalf("provider YAML: %s", p.YAML)
	}
	if _, err = PrepareWithImages("/tmp", "id", ledger.Manifest{Content: []byte(source)}, map[string]string{"web": "override"}); err == nil {
		t.Fatal("accepted override")
	}
}
func TestGitBuildInvalid(t *testing.T) {
	for _, body := range []string{
		"build: .", "image: test\n    x-launcher: {git: {repository: owner/repo, ref: main}}", "build: {args: [MISSING]}\n    x-launcher: {git: {repository: owner/repo, ref: main}}", "build: {secret: x}\n    x-launcher: {git: {repository: owner/repo, ref: main}}",
	} {
		src := "services:\n  web:\n    " + body + "\nx-launcher: {endpoint: {service: web, port: 80}}\n"
		if _, err := Prepare("/tmp", "id", ledger.Manifest{Content: []byte(src)}); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestPrebuiltRepositoryRequiresLiteralRepository(t *testing.T) {
	for _, value := range []any{
		nil, false, "", "team/web", "https://registry.example.test/team/web",
		"registry.example.test/team/web:latest", "registry.example.test/team/web@sha256:" + strings.Repeat("a", 64),
		"registry.example.test/team/$$WEB", " registry.example.test/team/web",
	} {
		git := map[string]any{"repository": "owner/repo", "ref": "main", "prebuilt_repository": value}
		if _, err := parseBuild(git, "."); err == nil || !strings.Contains(err.Error(), "prebuilt_repository") {
			t.Errorf("prebuilt_repository %v returned %v, want validation error", value, err)
		}
	}
}

func TestPrebuiltRequiresRelativeBuildPaths(t *testing.T) {
	for _, build := range []any{"/src", map[string]any{"dockerfile": "/src/Dockerfile"}} {
		git := map[string]any{"repository": "owner/repo", "ref": "main", "prebuilt_repository": "registry.example.test/team/web"}
		if _, err := parseBuild(git, build); err == nil || !strings.Contains(err.Error(), "must be relative paths") {
			t.Errorf("prebuilt configuration accepted build %v: %v", build, err)
		}
		delete(git, "prebuilt_repository")
		if _, err := parseBuild(git, build); err != nil {
			t.Errorf("build-only configuration rejected before checkout: %v", err)
		}
	}
}

func TestBuildSettingsAndArguments(t *testing.T) {
	for _, value := range []any{map[string]any{"A": "one", "B": "$$HOME"}, []any{"A=one", "B=$$HOME"}} {
		args, err := buildArgs(value)
		if err != nil || len(args) != 2 || args[1] != "B=$HOME" {
			t.Fatalf("args: %v %v", args, err)
		}
	}
	for _, value := range []any{map[string]any{"A": nil}, map[string]any{"": "x"}, []any{"A"}, 1} {
		if _, err := buildArgs(value); err == nil {
			t.Errorf("accepted %#v", value)
		}
	}
	for _, value := range []any{map[string]any{"context": 4}, 1, ""} {
		if _, err := parseBuild(map[string]any{"repository": "a/b", "ref": "main"}, value); err == nil {
			t.Errorf("accepted %#v", value)
		}
	}
	for _, git := range []any{nil, map[string]any{}, map[string]any{"repository": "a/b"}, map[string]any{"repository": "a/b", "ref": "x", "bad": 1}} {
		if _, err := parseBuild(git, "."); err == nil {
			t.Errorf("accepted %#v", git)
		}
	}
	b, err := parseBuild(map[string]any{"repository": "a/b", "ref": "a$$b"}, ".")
	if err != nil || b.Ref != "a$b" || b.PrebuiltRepository != "" {
		t.Fatalf("ref: %+v %v", b, err)
	}
}
