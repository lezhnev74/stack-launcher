package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockState excludes another launcher for the lifetime of the returned file.
func LockState(dir string) (*os.File, error) {
	path := filepath.Join(dir, ".launcher.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock state directory: %w", err)
	}
	return f, nil
}
