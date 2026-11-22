package podman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"stack-launcher.local/app/internal/runlog"
)

const commandTimeout = 2 * time.Minute

type runner struct {
	runID  string
	env    []string
	dir    string
	redact bool
}

func (r runner) logf(format string, args ...any) {
	if r.runID != "" {
		runlog.Printf(r.runID, format, args...)
	}
}

func cleanEnv(provider string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if isolatedEnv(key) {
			continue
		}
		env = append(env, item)
	}
	return append(env, "PODMAN_COMPOSE_PROVIDER="+provider)
}
func isolatedEnv(key string) bool {
	return strings.HasPrefix(key, "COMPOSE_") || strings.HasPrefix(key, "PODMAN_COMPOSE_") || key == "REGISTRY_AUTH_FILE" || key == "CONTAINER_HOST" || key == "CONTAINER_CONNECTION" || key == "DOCKER_HOST" || key == "DOCKER_CONTEXT"
}
func (r runner) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return r.runCommand(ctx, args...)
}

func (r runner) runBuild(ctx context.Context, args ...string) (string, error) {
	return r.execute(ctx, true, args...)
}
func (r runner) runCommand(ctx context.Context, args ...string) (string, error) {
	return r.execute(ctx, false, args...)
}
func (r runner) execute(ctx context.Context, build bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", args...) // #nosec G204 -- fixed executable and validated Compose inputs
	cmd.Env, cmd.Dir = r.env, r.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	configureBuild(cmd, build)
	cmd.Cancel = func() error {
		signal := cancelSignal(build)
		return syscall.Kill(-cmd.Process.Pid, signal)
	}
	cmd.WaitDelay = 2 * time.Second
	var output []byte
	var err error
	if build {
		var stderr bytes.Buffer
		cmd.Stdout = io.Discard
		cmd.Stderr = &limitedWriter{writer: &stderr, remaining: 16384}
		err = cmd.Run()
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, stderr.String())
		}
	} else if args[0] == "exec" {
		// Run with both output streams discarded because commands can print runtime secrets.
		err = cmd.Run()
	} else {
		if r.redact {
			cmd.Stderr = io.Discard
		}
		output, err = cmd.Output()
	}
	return commandResult(ctx, args[0], cmd, output, err)
}

func commandError(operation string, cmd *exec.Cmd, err error) error {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			return fmt.Errorf("podman %s: %w: %s", operation, err, stderr)
		}
	}
	return fmt.Errorf("podman %s: %w", operation, err)
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
	}
	if len(p) > 0 {
		_, _ = w.writer.Write(p)
		w.remaining -= len(p)
	}
	return n, nil
}

func configureBuild(cmd *exec.Cmd, build bool) {
	if build {
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	}
}
func cancelSignal(build bool) syscall.Signal {
	if build {
		return syscall.SIGTERM
	}
	return syscall.SIGKILL
}

func withContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.Join(err, ctx.Err())
	}
	return err
}

func commandResult(ctx context.Context, operation string, cmd *exec.Cmd, output []byte, err error) (string, error) {
	if err != nil {
		return "", commandError(operation, cmd, withContextError(ctx, err))
	}
	return strings.TrimSpace(string(output)), nil
}
