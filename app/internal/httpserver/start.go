package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/runlog"
)

func (a *api) startStack(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		runlog.Printf(id, "Start rejected reason=%q", operationConflict)
		http.Error(w, operationConflict, http.StatusConflict)
		return
	}
	s, accepted := a.claimStart(w, req, id)
	if !accepted {
		a.operations.Delete(id)
		return
	}
	runlog.Printf(id, "Start accepted hostname=%q", s.Hostname)
	go a.startWork(s) // #nosec G118 -- accepted work outlives the HTTP request
	w.Header().Set("Location", "/stacks/"+id)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func (a *api) claimStart(w http.ResponseWriter, req *http.Request, id string) (ledger.Status, bool) {
	s, err := a.db.Status(req.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
		return s, false
	}
	if err != nil {
		http.Error(w, "cannot read stack", http.StatusInternalServerError)
		return s, false
	}
	if !s.APIOwned || s.State != "stopped" {
		runlog.Printf(id, "Start rejected state=%q api_owned=%t", s.State, s.APIOwned)
		http.Error(w, "start requires a stopped API stack", http.StatusConflict)
		return s, false
	}
	if message := a.startIngressProblem(s.Hostname); message != "" {
		runlog.Printf(id, "Start rejected reason=%q", message)
		http.Error(w, message, http.StatusServiceUnavailable)
		return s, false
	}
	if err := a.db.BeginStart(req.Context(), id); err != nil {
		runlog.Printf(id, "Could not record starting state error=%q", err)
		http.Error(w, "cannot record starting state", http.StatusInternalServerError)
		return s, false
	}
	return s, true
}

func (a *api) startIngressProblem(host string) string {
	if a.disabledReason != "" {
		return a.disabledReason
	}
	if a.router == nil || !matchesBase(host, a.cfg.BaseDomain) {
		return "restore the stack's base_domain and original Caddy instance to start it"
	}
	return ""
}

func (a *api) startWork(s ledger.Status) {
	defer a.operations.Delete(s.ID)
	a.launchNote(s.ID, "Starting the saved Compose project with its pinned images.")
	result, err := a.start(a.launchContext(s.ID), a.cfg, a.db, s.ID)
	if err != nil {
		a.fail(s.ID, s.Hostname, "startup", err, false)
		return
	}
	// A delayed Stop PATCH must stay stale even if Start reuses the same port.
	runlog.Printf(s.ID, "Invalidating previous Caddy route updates hostname=%q", s.Hostname)
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	err = a.router.Abort(ctx, s.Hostname)
	cancel()
	if err != nil {
		a.fail(s.ID, s.Hostname, "publication", err, false)
		return
	}
	a.publish(s.ID, s.Hostname, result, s.Duration)
}
