package podman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stack-launcher.local/app/internal/compose"
)

func TestRemoveUsesProjectOwnershipAndDependencyOrder(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$REMOVE_TEST_LOG"
case "$1:$2" in
 info:--format) echo 'false true';;
 ps:-aq) printf 'base\ndependent\n';;
 rm:--force) :;;
 network:ls) echo test-network;;
 volume:ls) echo test-volume;;
 network:rm|volume:rm) :;;
 *) exit 2;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(script), 0700); err != nil { // #nosec G306 -- test-owned fake executable
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "commands")
	t.Setenv("REMOVE_TEST_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := Remove(t.Context(), "test-stack"); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(logPath) // #nosec G304 -- test-owned log
	if err != nil {
		t.Fatal(err)
	}
	filter := "label=com.docker.compose.project=" + compose.ProjectName("test-stack")
	want := []string{
		"info --format {{.Host.ServiceIsRemote}} {{.Host.Security.Rootless}}",
		"ps -aq --no-trunc --sort created --filter " + filter,
		"rm --force --ignore --volumes -- dependent",
		"rm --force --ignore --volumes -- base",
		"network ls -q --filter " + filter,
		"network rm -- test-network",
		"volume ls -q --filter " + filter,
		"volume rm -- test-volume",
	}
	if got := strings.TrimSpace(string(commands)); got != strings.Join(want, "\n") {
		t.Fatalf("unexpected teardown commands:\n%s", got)
	}
}
