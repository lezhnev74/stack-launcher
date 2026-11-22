package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
)

type environmentInput ledger.Environment

func (e *environmentInput) UnmarshalJSON(data []byte) error {
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return errors.New("environment must be a mapping")
	}
	env := environmentInput{}
	for service, values := range raw {
		if values == nil {
			return errors.New("environment service must be a mapping")
		}
		parsed, err := parseEnvValues(values)
		if err != nil {
			return err
		}
		env[service] = parsed
	}
	*e = env
	return nil
}
func parseEnvValues(values map[string]json.RawMessage) (map[string]string, error) {
	result := map[string]string{}
	for key, value := range values {
		var text string
		if string(value) == "null" || json.Unmarshal(value, &text) != nil {
			return nil, errors.New("environment values must be strings")
		}
		result[key] = text
	}
	return result, nil
}
func (a *api) getEnvironment(w http.ResponseWriter, req *http.Request) {
	env, err := a.db.GetEnvironment(req.Context(), req.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, "cannot read environment", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"environment": env})
}
func (a *api) putEnvironment(w http.ResponseWriter, req *http.Request) {
	env, err := decodeEnvironment(w, req)
	if err != nil {
		inputError(w, err)
		return
	}
	id := req.PathValue("id")
	if _, loaded := a.operations.LoadOrStore(id, struct{}{}); loaded {
		http.Error(w, operationConflict, http.StatusConflict)
		return
	}
	defer a.operations.Delete(id)
	a.writeEnvironment(w, req, id, env)
}

func (a *api) writeEnvironment(w http.ResponseWriter, req *http.Request, id string, env ledger.Environment) {
	if !a.environmentEditable(w, req, id) {
		return
	}
	if len(env) > 0 && !a.validateSavedServices(w, req, id, env) {
		return
	}
	if err := a.db.SetEnvironment(req.Context(), id, env); err != nil {
		http.Error(w, "cannot save environment", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a *api) environmentEditable(w http.ResponseWriter, req *http.Request, id string) bool {
	status, err := a.db.Status(req.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
		return false
	}
	if err != nil {
		http.Error(w, "cannot read stack", http.StatusInternalServerError)
		return false
	}
	if !status.APIOwned || status.State != "draft" {
		http.Error(w, "stack environment can only be changed while the run is a draft", http.StatusConflict)
		return false
	}
	return true
}
func (a *api) validateSavedServices(w http.ResponseWriter, req *http.Request, id string, env ledger.Environment) bool {
	manifest, err := a.db.GetManifest(req.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "stack has no manifest", http.StatusConflict)
		return false
	}
	if err != nil {
		http.Error(w, "cannot read manifest", http.StatusInternalServerError)
		return false
	}
	if err := compose.ValidateEnvironmentServices(manifest, env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
func decodeEnvironment(w http.ResponseWriter, req *http.Request) (ledger.Environment, error) {
	var input struct {
		Environment *environmentInput `json:"environment"`
	}
	if err := decodeJSON(w, req, &input); err != nil {
		return nil, err
	}
	if input.Environment == nil {
		return nil, errors.New("environment is required")
	}
	env := ledger.Environment(*input.Environment)
	return env, compose.ValidateEnvironment(env)
}
