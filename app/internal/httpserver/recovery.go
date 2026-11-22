package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/runlog"
)

// recover only touches API-owned records; older internal callers can retry.
func (a *api) recover(ctx context.Context) error {
	records, err := a.db.APIRecords(ctx)
	if err != nil {
		return err
	}
	routeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var recordErr error
	for _, s := range records {
		recordErr = errors.Join(recordErr, a.recoverRecord(ctx, routeCtx, s))
	}
	if recordErr != nil {
		return recordErr
	}
	if a.router != nil {
		a.checkIngress(records)
	}
	return a.sweep(ctx)
}

func (a *api) checkIngress(records []ledger.LaunchRecord) {
	for _, s := range records {
		if (s.State == "ready" || s.State == "starting" || s.State == "stopping" || s.Pending) && !matchesBase(s.Hostname, a.cfg.BaseDomain) {
			a.disabledReason = fmt.Sprintf("stack %s belongs to a different base domain. Restore its base_domain", s.ID)
			runlog.Printf(s.ID, "Launches disabled reason=%q", a.disabledReason)
			return
		}
	}
}
func matchesBase(host, base string) bool {
	label, ok := strings.CutSuffix(host, "."+base)
	return ok && label != "" && !strings.Contains(label, ".")
}
func (a *api) recoverRecord(ctx, routeCtx context.Context, s ledger.LaunchRecord) error {
	var err error
	switch s.State {
	case "launching":
		runlog.Printf(s.ID, "Recovering interrupted launch publication_pending=%t", s.Pending)
		_, err = a.db.Fail(ctx, s.ID, "interrupted", "launcher interrupted during launch")
	case "starting":
		runlog.Printf(s.ID, "Recovering interrupted start publication_pending=%t", s.Pending)
		_, err = a.db.Fail(ctx, s.ID, "interrupted", "launcher interrupted during start")
	}
	if s.Pending && s.State != "ready" {
		a.withdrawPublication(routeCtx, s.ID, s.Hostname)
	}
	return err
}

func (a *api) withdrawPublication(ctx context.Context, id, host string) {
	if a.router != nil && matchesBase(host, a.cfg.BaseDomain) {
		a.abort(ctx, id, host)
	}
}

func (a *api) withdrawFailedRecord(ctx context.Context, id string) {
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	defer a.operations.Delete(id)
	s, err := a.db.Status(ctx, id)
	if err == nil && s.State == "failed" && s.Pending {
		a.withdrawPublication(ctx, id, s.Hostname)
	}
}

func (a *api) retryWithdrawals(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.withdrawFailed(ctx)
		}
	}
}

func (a *api) withdrawFailed(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, recordTimeout)
	defer cancel()
	records, err := a.db.APIRecords(ctx)
	if err != nil {
		log.Printf("read pending publications: %v", err)
		return
	}
	for _, s := range records {
		if s.State == "failed" && s.Pending {
			a.withdrawFailedRecord(ctx, s.ID)
		}
	}
}
func (a *api) sweep(ctx context.Context) error {
	owned, err := a.db.Checkouts(ctx)
	if err != nil {
		return err
	}
	if err := sweepCheckouts(filepath.Join(a.cfg.StateDir, "checkouts"), owned); err != nil {
		return err
	}
	return sweepDir(a.cfg.StateDir, ".compose-", nil)
}
func sweepCheckouts(dir string, keep map[string]bool) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return sweepDir(dir, "checkout-", keep)
}
func sweepDir(dir, prefix string, keep map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	return removeEntries(dir, prefix, keep, entries)
}
func removeEntries(dir, prefix string, keep map[string]bool, entries []os.DirEntry) error {
	for _, entry := range entries {
		if entry.IsDir() && temporaryName(entry.Name(), prefix) && !keep[entry.Name()] {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func temporaryName(name, prefix string) bool {
	suffix, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	n, err := strconv.ParseUint(suffix, 10, 32)
	// os.MkdirTemp in the pinned Go toolchain uses the decimal uint32 spelling.
	return err == nil && strconv.FormatUint(n, 10) == suffix
}
