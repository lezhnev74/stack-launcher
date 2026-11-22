package httpserver

import (
	"context"
	"net/http"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/manifest"
	"stack-launcher.local/app/internal/runlog"
)

func (a *api) retryStack(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		http.Error(w, operationConflict, http.StatusConflict)
		return
	}
	s, accepted := a.claimRetry(w, req, id)
	if !accepted {
		a.operations.Delete(id)
		return
	}
	runlog.Printf(id, "Launch retry accepted hostname=%q manifest_saved=%t images_saved=%t", s.Hostname, s.HasManifest, s.HasImages)
	go a.retryWork(s) // #nosec G118 -- accepted work outlives the HTTP request
	acceptLaunch(w, id)
}

func (a *api) claimRetry(w http.ResponseWriter, req *http.Request, id string) (ledger.Status, bool) {
	ctx := req.Context()
	s, err := a.db.Status(ctx, id)
	if err != nil {
		recordError(w, req, err, "cannot read run")
		return s, false
	}
	if !s.Retryable {
		http.Error(w, "retry requires a failed run with saved launch inputs", http.StatusConflict)
		return s, false
	}
	if message := a.startIngressProblem(s.Hostname); message != "" {
		http.Error(w, message, http.StatusServiceUnavailable)
		return s, false
	}
	if s.Pending && !a.fenceRetry(w, ctx, s) {
		return s, false
	}
	if err := a.db.BeginRetry(ctx, id); err != nil {
		runlog.Printf(id, "Could not record launch retry error=%q", err)
		http.Error(w, "cannot record retry state", http.StatusInternalServerError)
		return s, false
	}
	return s, true
}

func (a *api) fenceRetry(w http.ResponseWriter, ctx context.Context, s ledger.Status) bool {
	if !a.fence(ctx, s.ID, s.Hostname) {
		http.Error(w, "cannot withdraw the previous Caddy route", http.StatusBadGateway)
		return false
	}
	if err := a.db.ClearPending(ctx, s.ID); err != nil {
		runlog.Printf(s.ID, "Could not record route withdrawal before retry error=%q", err)
		http.Error(w, "cannot record route withdrawal", http.StatusInternalServerError)
		return false
	}
	return true
}

func (a *api) retryWork(s ledger.Status) {
	a.launchNote(s.ID, "Retrying launch with the saved configuration.")
	if s.HasImages {
		defer a.operations.Delete(s.ID)
		result, err := a.retryPinned(a.launchContext(s.ID), a.cfg, a.db, s.ID)
		a.completeLaunch(s.ID, s.Hostname, s.Duration, result, err)
		return
	}
	runlog.Printf(s.ID, "Loading saved launch request")
	request, err := a.db.LaunchRequest(a.ctx, s.ID)
	if err != nil {
		a.fail(s.ID, s.Hostname, "acquisition", err, false)
		a.operations.Delete(s.ID)
		return
	}
	if s.HasManifest {
		runlog.Printf(s.ID, "Reusing saved manifest for launch retry")
		a.launchWork(s.ID, s.Hostname, request.Images, s.Duration)
		return
	}
	source := manifest.Input{Reference: request.Reference, InlineYAML: request.InlineYAML}
	a.work(s.ID, s.Hostname, source, request.Images, s.Duration)
}
