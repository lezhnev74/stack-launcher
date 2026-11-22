package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stack-launcher.local/app/internal/ledger"
)

func TestRuntimeEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "env"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env", "defaults"), []byte("SECRET=marker\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := []byte("services:\n  web:\n    image: example\n    env_file: defaults\n    environment: [A=manifest, B=keep, TOKEN$5=old]\nx-launcher:\n  endpoint: {service: web, port: 80}\n")
	original := string(source)
	manifest := ledger.Manifest{Content: source}
	got, err := PrepareRuntime(dir, "id", manifest, nil, ledger.Environment{"web": {"A": "$literal", "EMPTY": "", "TOKEN$5": "new"}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got.YAML)
	for _, want := range []string{"$$literal", "EMPTY: \"\"", "B: keep", "TOKEN$$5: new", filepath.Join(dir, "env", "defaults")} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "marker") || strings.Contains(text, "TOKEN$5: old") || string(manifest.Content) != original {
		t.Fatal("defaults or source changed")
	}
	if _, err := PrepareRuntime(dir, "id", manifest, nil, ledger.Environment{"missing": {"A": "value"}}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("unknown environment service: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "env", "defaults")); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareRuntime(dir, "id", manifest, nil, nil); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := PrepareForStop(dir, "id", manifest, nil); err != nil {
		t.Fatal(err)
	}
}
func TestEnvironmentPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "env"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "env", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "env", "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env", "unreadable"), []byte("x"), 0000); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside", "link", "missing", ".", "a$b", "directory", "unreadable"} {
		if _, err := envPath(dir, name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}
