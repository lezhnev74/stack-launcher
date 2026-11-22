package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, data, listen, failure string }{
		{"default", "state_dir: state\n", "127.0.0.1:8080", ""},
		{"explicit", "listen: ':9000'\nstate_dir: state\n", ":9000", ""},
		{"null", "listen: null\nstate_dir: state\n", "", "listen"},
		{"empty listen", "listen: ''\nstate_dir: state\n", "", "listen"},
		{"removed github", "state_dir: state\ngithub: {}\n", "", `unknown key "github"`},
		{"unknown", "state_dir: state\nextra: yes\n", "", `unknown key "extra"`},
		{"merge", "state_dir: state\n<<: {}\n", "", `unknown key "<<"`},
		{"tagged key", "!custom state_dir: state\n", "", `unknown key "state_dir"`},
		{"duplicate", "state_dir: missing\nstate_dir: state\n", "", `duplicate key "state_dir"`},
		{"non-mapping", "[state_dir, state]\n", "", "expected a YAML mapping"},
		{"malformed", "[\n", "", "config"},
		{"empty", "", "", "config"},
		{"second", "state_dir: state\n---\nfoo: bar\n", "", "document"},
		{"separator", "state_dir: state\n---\n", "", "document"},
		{"missing state", "listen: ':1'\n", "", "state_dir"},
		{"invalid state type", "state_dir: false\n", "", "state_dir must be a non-empty string"},
		{"absent", "state_dir: absent\n", "", "state_dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, "config.yml")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) || !strings.Contains(err.Error(), path) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || cfg.Listen != tc.listen || cfg.StateDir != state {
				t.Fatalf("config = %+v, error = %v", cfg, err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Fatalf("absent directory: %v", err)
	}
	entries, err := os.ReadDir(state)
	if err != nil || len(entries) != 0 {
		t.Fatalf("state contents: %v, %v", entries, err)
	}
	if _, err := Load(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestStateDirectory(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config")
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "wrong")
	if err := os.Mkdir(dir, 0755); err != nil { // #nosec G301 -- test rejects an insecure mode
		t.Fatal(err)
	}
	for _, target := range []string{file, dir} {
		if err := os.WriteFile(path, []byte("state_dir: "+target+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "state_dir") {
			t.Fatalf("%s: %v", target, err)
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("mode changed: %v %v", info, err)
	}
}

func TestProviderAndAuthPaths(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yml")
	for _, tc := range []struct {
		extra, provider, auth string
		bad                   bool
	}{
		{"", "podman-compose", "", false},
		{"compose_provider: custom\nregistry_auth_file: auth.json\n", "custom", filepath.Join(root, "auth.json"), false},
		{"compose_provider: ./bin/compose\n", filepath.Join(root, "bin/compose"), "", false},
		{"compose_provider: ' ' \n", "", "", true},
		{"registry_auth_file: null\n", "", "", true},
	} {
		if err := os.WriteFile(path, []byte("state_dir: state\n"+tc.extra), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if tc.bad {
			if err == nil {
				t.Fatalf("accepted %q", tc.extra)
			}
			continue
		}
		if err != nil || cfg.ComposeProvider != tc.provider || cfg.RegistryAuthFile != tc.auth {
			t.Fatalf("%q: %+v %v", tc.extra, cfg, err)
		}
	}
}

func TestCaddyConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yml")
	for _, tc := range []struct{ extra, failure string }{
		{"base_domain: EXAMPLE.com\ncaddy_admin_socket: caddy.sock\n", ""},
		{"base_domain: example.com\n", "caddy_admin_socket"},
		{"caddy_admin_socket: caddy.sock\n", "base_domain"},
		{"base_domain: '*.example.com'\ncaddy_admin_socket: caddy.sock\n", "base_domain"},
		{"base_domain: example.com\ncaddy_admin_socket: ''\n", "caddy_admin_socket"},
		{"base_domain: example.com\ncaddy_admin_socket: 'http://localhost'\n", "caddy_admin_socket"},
	} {
		if err := os.WriteFile(path, []byte("state_dir: state\n"+tc.extra), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if tc.failure != "" {
			if err == nil || !strings.Contains(err.Error(), tc.failure) {
				t.Fatalf("%s: %v", tc.extra, err)
			}
			continue
		}
		if err != nil || cfg.BaseDomain != "example.com" || cfg.CaddyAdminSocket != filepath.Join(root, "caddy.sock") {
			t.Fatalf("%+v %v", cfg, err)
		}
	}
}
