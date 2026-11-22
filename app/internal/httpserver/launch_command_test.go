package httpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stack-launcher.local/app/internal/podman"
)

func TestLaunchCommandGatesPublication(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			a, router := setupAPI(t)
			dir := t.TempDir()
			script := `#!/bin/sh
case "$1" in
 info) echo 'false true';;
 pull) echo sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa;;
 compose) while [ "$#" -gt 0 ]; do case "$1" in --project-name) echo "$2" > "$FAKE_DIR/project"; shift;; down) touch "$FAKE_DIR/stopped";; esac; shift; done;;
 ps) if [ ! -f "$FAKE_DIR/stopped" ]; then echo 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef; fi;;
 container) project=$(cat "$FAKE_DIR/project"); printf '[{"Id":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","Image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Config":{"Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"web"}},"State":{"Status":"running"},"NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"12345"}]}}}]' "$project";;
 exec) printf '%s\n' "$@" > "$FAKE_DIR/args"; touch "$FAKE_DIR/entered"; while [ ! -f "$FAKE_DIR/release" ]; do sleep .01; done; if [ -f "$FAKE_DIR/fail" ]; then echo super-secret-command-output >&2; exit 7; fi;;
 network) :;;
 *) exit 2;;
esac
`
			for _, name := range []string{"podman", "podman-compose"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil { // #nosec G306 -- test-owned fake executable
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_DIR", dir)
			if fail {
				if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			a.launch = podman.Launch
			manifest := `services:
  web:
    image: example/web:latest
    x-launcher:
      launch_command: [sh, -c, 'echo $$VALUE']
x-launcher:
  endpoint: {service: web, port: 8080, https: false}
`
			body, _ := json.Marshal(map[string]string{"inline_yaml": manifest})
			id := postID(t, a, string(body))
			defer func() { _ = os.WriteFile(filepath.Join(dir, "release"), nil, 0600) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "entered")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					s, _ := a.db.Status(t.Context(), id)
					t.Fatalf("command did not start: %+v", s)
				}
				time.Sleep(time.Millisecond)
			}
			w := serve(a, "GET", "/stacks/"+id, "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"launching"`) {
				t.Fatalf("while blocked: %d %s", w.Code, w.Body)
			}
			if p, n := router.counts(); p != 0 || n != 0 {
				t.Fatalf("while blocked: publish=%d abort=%d", p, n)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args")) // #nosec G304 -- test-owned temporary fixture
			if err != nil {
				t.Fatal(err)
			}
			if string(args) != "exec\n--\n0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\nsh\n-c\necho $VALUE\n" {
				t.Fatalf("exec argv: %q", args)
			}
			if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if fail {
				s := waitState(t, a, id, "failed")
				w = serve(a, "GET", "/stacks/"+id, "")
				if s.FailurePhase != "startup" || !strings.Contains(s.Failure, "web") || !strings.Contains(s.Failure, "exit status 7") || strings.Contains(s.Failure, "cleanup") || w.Code != 200 || s.URL != "" || strings.Contains(w.Body.String(), "super-secret-command-output") {
					t.Fatalf("failure: %+v response=%s", s, w.Body)
				}
				if p, n := router.counts(); p != 0 || n != 0 {
					t.Fatalf("failure: publish=%d abort=%d", p, n)
				}
			} else {
				s := waitState(t, a, id, "ready")
				if s.URL == "" {
					t.Fatal("ready without URL")
				}
				if p, n := router.counts(); p != 1 || n != 0 {
					t.Fatalf("success: publish=%d abort=%d", p, n)
				}
			}
		})
	}
}
