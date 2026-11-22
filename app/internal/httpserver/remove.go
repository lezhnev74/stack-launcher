package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/runlog"
)

func (a *api) removeStack(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		http.Error(w, operationConflict, http.StatusConflict)
		return
	}
	defer a.operations.Delete(id)
	s, err := a.db.Status(req.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, "cannot read stack", http.StatusInternalServerError)
		return
	}
	if code, message := a.removeEligibility(s); code != 0 {
		http.Error(w, message, code)
		return
	}
	if code, message := a.completeRemove(req.Context(), s); code != 0 {
		http.Error(w, message, code)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) removeEligibility(s ledger.Status) (int, string) {
	if !s.APIOwned {
		return http.StatusConflict, "remove requires a stack created through the API"
	}
	switch s.State {
	case "draft", "ready", "stopping", "stopped", "failed", "removing":
	default:
		return http.StatusConflict, "cannot remove stack in state " + s.State
	}
	if needsRemoveFence(s) && (a.router == nil || !matchesBase(s.Hostname, a.cfg.BaseDomain)) {
		return http.StatusServiceUnavailable, "restore base_domain and caddy_admin_socket for the stack's original Caddy instance to remove it"
	}
	return 0, ""
}

func needsRemoveFence(s ledger.Status) bool {
	return s.Pending || s.State == "ready" || s.State == "stopping"
}

func (a *api) completeRemove(ctx context.Context, s ledger.Status) (int, string) {
	if err := a.db.BeginRemove(ctx, s.ID); err != nil {
		return removeError(s.ID, "record removing state", err)
	}
	if needsRemoveFence(s) {
		if code, message := a.fenceRemoval(ctx, s); code != 0 {
			return code, message
		}
	}
	return a.removeAssets(ctx, s.ID)
}

func (a *api) fenceRemoval(ctx context.Context, s ledger.Status) (int, string) {
	if err := a.router.Abort(ctx, s.Hostname); err != nil {
		runlog.Printf(s.ID, "Could not withdraw Caddy route before removal error=%q", err)
		return http.StatusBadGateway, "cannot withdraw Caddy route. Retry remove"
	}
	if err := a.db.ClearPending(ctx, s.ID); err != nil {
		return removeError(s.ID, "record route withdrawal", err)
	}
	return 0, ""
}

func (a *api) removeAssets(ctx context.Context, id string) (int, string) {
	hasProject, checkout, err := a.db.RemovalAssets(ctx, id)
	if err != nil {
		return removeError(id, "read removal assets", err)
	}
	if hasProject {
		if err := a.remove(ctx, id); err != nil {
			return removeError(id, "remove Podman project", err)
		}
	}
	if err := removeCheckout(a.cfg.StateDir, checkout); err != nil {
		return removeError(id, "remove checkout", err)
	}
	if err := a.db.FinishRemove(ctx, id); err != nil {
		return removeError(id, "delete stack record", err)
	}
	return 0, ""
}

func removeError(id, operation string, err error) (int, string) {
	runlog.Printf(id, "Remove failed operation=%q error=%q", operation, err)
	return http.StatusInternalServerError, "cannot " + operation + ". Retry remove"
}

func removeCheckout(stateDir, name string) error {
	if name == "" {
		return nil
	}
	if !temporaryName(name, "checkout-") {
		return errors.New("invalid recorded checkout name")
	}
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return removeRootCheckout(root, name)
}

func removeRootCheckout(root *os.Root, name string) error {
	info, err := root.Lstat("checkouts")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("checkouts path is not a directory")
	}
	return root.RemoveAll(filepath.Join("checkouts", name))
}
