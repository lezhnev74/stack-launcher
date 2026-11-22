package httpserver

import (
	"context"
	"net/http"
	"time"

	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/runlog"
)

func (a *api) createDraft(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Template string `json:"template"`
	}
	if err := decodeJSON(w, req, &in); err != nil {
		inputError(w, err)
		return
	}
	if !validTemplateName(in.Template) {
		http.Error(w, "choose a template", http.StatusBadRequest)
		return
	}
	value, err := a.db.ManifestTemplate(req.Context(), in.Template)
	if err != nil {
		recordError(w, req, err, "cannot read template")
		return
	}
	id, err := reserveStackName(time.Now(), func(id string) error {
		return a.db.CreateDraft(req.Context(), id, in.Template, defaultDuration, value)
	})
	if err != nil {
		http.Error(w, "cannot create run", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Location", "/ui/stacks?id="+id)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (a *api) updateDraft(w http.ResponseWriter, req *http.Request) {
	a.writeDraft(w, req, false)
}

func (a *api) launchDraft(w http.ResponseWriter, req *http.Request) {
	switch {
	case a.disabledReason != "":
		http.Error(w, a.disabledReason, http.StatusServiceUnavailable)
	case !a.validLaunchDomain():
		http.Error(w, errBaseDomainLength.Error(), http.StatusServiceUnavailable)
	default:
		a.writeDraft(w, req, true)
	}
}

func (a *api) writeDraft(w http.ResponseWriter, req *http.Request, launch bool) {
	var in configurationInput
	if err := decodeJSON(w, req, &in); err != nil {
		inputError(w, err)
		return
	}
	id := req.PathValue("id")
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		http.Error(w, operationConflict, http.StatusConflict)
		return
	}
	if !a.commitDraft(w, req, id, in, launch) {
		a.operations.Delete(id)
	}
}

// commitDraft reports whether a launch worker has taken ownership of the operation.
func (a *api) commitDraft(w http.ResponseWriter, req *http.Request, id string, in configurationInput, launch bool) bool {
	status, err := a.db.Status(req.Context(), id)
	if err != nil {
		recordError(w, req, err, "cannot read run")
		return false
	}
	if !status.APIOwned || status.State != "draft" {
		http.Error(w, "run is no longer a draft", http.StatusConflict)
		return false
	}
	host := ""
	if launch {
		host = id + "." + a.cfg.BaseDomain
	}
	if !a.saveDraft(w, req, id, host, in) {
		return false
	}
	if launch {
		runlog.Printf(id, "Launch accepted source=draft hostname=%q duration=%s", host, status.Duration)
		go a.launchWork(id, host, nil, status.Duration) // #nosec G118 -- accepted work outlives the HTTP request
		acceptLaunch(w, id)
		return true
	}
	w.WriteHeader(http.StatusNoContent)
	return false
}

func (a *api) saveDraft(w http.ResponseWriter, req *http.Request, id, host string, in configurationInput) bool {
	ctx := req.Context()
	source, env, err := a.readDraftConfiguration(ctx, id, in)
	if err != nil {
		recordError(w, req, err, "cannot read run configuration")
		return false
	}
	if err := validateConfiguration(source, env); err != nil {
		inputError(w, err)
		return false
	}
	if err := a.db.SaveDraft(ctx, id, host, source, env); err != nil {
		http.Error(w, "cannot save run", http.StatusInternalServerError)
		return false
	}
	return true
}

func (a *api) readDraftConfiguration(ctx context.Context, id string, in configurationInput) (ledger.Manifest, ledger.Environment, error) {
	var original []byte
	if in.InlineYAML == nil {
		saved, err := a.db.GetManifest(ctx, id)
		if err != nil {
			return ledger.Manifest{}, nil, err
		}
		original = saved.Content
	}
	if in.Environment == nil {
		saved, err := a.db.GetEnvironment(ctx, id)
		if err != nil {
			return ledger.Manifest{}, nil, err
		}
		in.Environment = environmentInput(saved)
	}
	return in.manifest(original), ledger.Environment(in.Environment), nil
}
