package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/runlog"
)

func (a *api) stopStack(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		runlog.Printf(id, "Stop rejected reason=%q", operationConflict)
		http.Error(w, operationConflict, http.StatusConflict)
		return
	}
	defer a.operations.Delete(id)
	ctx := req.Context()
	s, err := a.db.Status(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, "cannot read stack", http.StatusInternalServerError)
		return
	}
	if s.APIOwned && s.State == "stopped" {
		runlog.Printf(id, "Run already stopped")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if code, message := a.stopEligibility(s); code != 0 {
		runlog.Printf(id, "Stop rejected reason=%q", message)
		http.Error(w, message, code)
		return
	}
	runlog.Printf(id, "Stop accepted state=%q hostname=%q", s.State, s.Hostname)
	a.completeStop(ctx, w, s)
}

func (a *api) stopEligibility(s ledger.Status) (int, string) {
	switch {
	case !s.APIOwned:
		return http.StatusConflict, "stop requires a stack created through the API"
	case s.State == "launching":
		return http.StatusConflict, "stack is launching. Wait for it to be ready before stopping"
	case s.State != "ready" && s.State != "stopping":
		return http.StatusConflict, "cannot stop stack in state " + s.State
	case a.router == nil:
		return http.StatusServiceUnavailable, "restore base_domain and caddy_admin_socket to stop this stack"
	case !matchesBase(s.Hostname, a.cfg.BaseDomain):
		return http.StatusServiceUnavailable, "restore the stack's base_domain and original Caddy instance to stop it"
	}
	return 0, ""
}

func (a *api) completeStop(ctx context.Context, w http.ResponseWriter, s ledger.Status) {
	code, message := a.withdrawStack(ctx, s)
	if code == 0 {
		code, message = a.stopProject(ctx, s.ID)
	}
	if code != 0 {
		http.Error(w, message, code)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) withdrawStack(ctx context.Context, s ledger.Status) (int, string) {
	started := time.Now()
	if s.State == "ready" {
		runlog.Printf(s.ID, "Recording stopping state")
		if err := a.db.BeginStop(ctx, s.ID); err != nil {
			runlog.Printf(s.ID, "Could not record stopping state error=%q", err)
			return http.StatusInternalServerError, "cannot record stopping state. Retry stop"
		}
	}
	runlog.Printf(s.ID, "Withdrawing Caddy route hostname=%q publication_pending=%t", s.Hostname, s.Pending)
	if err := a.withdrawForStop(ctx, s); err != nil {
		runlog.Printf(s.ID, "Could not withdraw Caddy route error=%q elapsed=%s", err, time.Since(started))
		return http.StatusBadGateway, "cannot withdraw Caddy route. Retry stop"
	}
	runlog.Printf(s.ID, "Caddy route withdrawn elapsed=%s", time.Since(started))
	return 0, ""
}

func (a *api) stopProject(ctx context.Context, id string) (int, string) {
	started := time.Now()
	runlog.Printf(id, "Stopping Podman project")
	if err := a.stop(ctx, a.cfg, a.db, id); err != nil {
		runlog.Printf(id, "Could not stop Podman project error=%q elapsed=%s", err, time.Since(started))
		return http.StatusInternalServerError, "cannot stop Podman project. Retry stop"
	}
	runlog.Printf(id, "Podman project stopped, recording stopped state")
	if err := a.db.FinishStop(ctx, id); err != nil {
		runlog.Printf(id, "Could not record stopped state error=%q", err)
		return http.StatusInternalServerError, "cannot record stopped state. Retry stop"
	}
	runlog.Printf(id, "Run stopped elapsed=%s", time.Since(started))
	return 0, ""
}

func (a *api) withdrawForStop(ctx context.Context, s ledger.Status) error {
	if s.Pending {
		return a.router.Abort(ctx, s.Hostname)
	}
	return a.router.Withdraw(ctx, s.Hostname)
}
