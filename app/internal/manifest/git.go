package manifest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

type gitRunner struct{ stateDir string }

const sshCommand = "ssh -o BatchMode=yes"

func gitEnvironment(ceiling string) []string {
	env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_")
	})
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+sshCommand,
		"GIT_ALLOW_PROTOCOL=ssh",
		"GIT_CEILING_DIRECTORIES="+ceiling,
	)
}

func (r gitRunner) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ceiling := ""
	if dir == "" {
		tmp, err := r.newScratchDir()
		if err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
		ceiling = tmp
	}
	cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- arguments are validated and passed directly
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Dir = dir
	cmd.Env = gitEnvironment(ceiling)
	cmd.WaitDelay = 2 * time.Second // Bound the wait for pipes inherited by SSH after cancellation.
	output, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s: %s: %w", args[0], strings.TrimSpace(string(exit.Stderr)), err)
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return output, nil
}

func (r gitRunner) newScratchDir() (string, error) {
	stateDir, err := filepath.Abs(r.stateDir)
	if err != nil {
		return "", err
	}
	return newCheckout(stateDir)
}
