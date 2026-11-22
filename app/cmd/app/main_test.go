package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestRunRequiresConfigPath(t *testing.T) {
	t.Parallel()
	if err := run(""); err == nil || !strings.Contains(err.Error(), "-c config path is required") {
		t.Fatalf("run without config path: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing.yml")
	if err := run(missing); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("run with missing config: %v", err)
	}
}

func TestRunReportsBindError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yml")
	if err := os.Symlink(state, filepath.Join(root, "state-link")); err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	for _, name := range []string{"state", "state-link"} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte("state_dir: "+name+"\nlisten: "+address+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := run(path)
			if !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("run with occupied address: %v", err)
			}
			for _, want := range []string{path, "listen", address} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("startup error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestRunReportsLedgerError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(state, "stacks.sqlite3")
	if err := os.WriteFile(db, []byte("invalid database"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yml")
	if err := os.WriteFile(path, []byte("state_dir: state\nlisten: 127.0.0.1:-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := run(path)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), db) {
		t.Fatalf("startup error: %v", err)
	}
}
