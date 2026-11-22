package httpserver

import (
	"context"
	"net/http"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/podman"
	"stack-launcher.local/app/internal/runlog"
)

func (a *api) launchContext(id string) context.Context {
	return podman.WithLaunchProgress(context.Background(), func(message string) {
		a.launchNote(id, message)
	})
}

func (a *api) launchNote(id, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := a.db.RecordLaunchActivity(ctx, id, message); err != nil {
		runlog.Printf(id, "Could not record launch activity error=%q", err)
	}
}

func (a *api) webLaunchActivity(w http.ResponseWriter, req *http.Request) {
	ctx, id := req.Context(), req.URL.Query().Get("id")
	status, err := a.db.Status(ctx, id)
	if err != nil {
		recordError(w, req, err, "cannot read run")
		return
	}
	events, err := a.db.LaunchHistory(ctx, id)
	if err != nil {
		recordError(w, req, err, "cannot read launch history")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, struct {
		State       string                  `json:"state"`
		HasManifest bool                    `json:"has_manifest"`
		Activity    []ledger.LaunchActivity `json:"activity"`
	}{State: status.State, HasManifest: status.HasManifest, Activity: events})
}

func (a *api) webRunManifest(w http.ResponseWriter, req *http.Request) {
	source, err := a.db.GetManifest(req.Context(), req.URL.Query().Get("id"))
	if err != nil {
		recordError(w, req, err, "cannot read run manifest")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(source.Content)
}
