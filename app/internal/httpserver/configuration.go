package httpserver

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
)

type configurationInput struct {
	InlineYAML  *string                             `json:"inline_yaml"`
	Environment environmentInput                    `json:"environment"`
	References  map[string]ledger.ReferenceOverride `json:"references"`
}

type templateInput struct {
	Name *string `json:"name"`
	configurationInput
}

func (in templateInput) nameOr(fallback string) string {
	if in.Name != nil {
		return *in.Name
	}
	return fallback
}

func (in configurationInput) manifest(content []byte) ledger.Manifest {
	if in.InlineYAML != nil {
		content = []byte(*in.InlineYAML)
	}
	return ledger.Manifest{Content: content, References: in.References}
}

func inspectManifest(source ledger.Manifest) ([]compose.ServiceReference, error) {
	if len(source.Content) > maxTemplateSize {
		return nil, &http.MaxBytesError{Limit: maxTemplateSize}
	}
	return compose.TemplateServices(source)
}

func validateConfiguration(source ledger.Manifest, env ledger.Environment) error {
	if err := validateReferences(source.References); err != nil {
		return err
	}
	if _, err := inspectManifest(source); err != nil {
		return err
	}
	if err := compose.ValidateEnvironment(env); err != nil {
		return err
	}
	return compose.ValidateEnvironmentServices(source, env)
}

func validateReferences(refs map[string]ledger.ReferenceOverride) error {
	for name, ref := range refs {
		if !validRefOverride(ref) {
			return fmt.Errorf("invalid reference override for service %q", name)
		}
	}
	return nil
}

func validRefOverride(ref ledger.ReferenceOverride) bool {
	switch ref.Kind {
	case "image":
		return validImage(ref.Value) && !strings.ContainsRune(ref.Value, 0)
	case "git":
		return strings.TrimSpace(ref.Value) != "" && !strings.ContainsAny(ref.Value, "\x00\r\n")
	default:
		return false
	}
}

func inspectServices(w http.ResponseWriter, req *http.Request) {
	var in struct {
		InlineYAML string `json:"inline_yaml"`
	}
	if err := decodeJSON(w, req, &in); err != nil {
		inputError(w, err)
		return
	}
	services, err := inspectManifest(ledger.Manifest{Content: []byte(in.InlineYAML)})
	if err != nil {
		inputError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, services)
}

func (a *api) createTemplate(w http.ResponseWriter, req *http.Request) {
	var in templateInput
	if err := decodeJSON(w, req, &in); err != nil {
		inputError(w, err)
		return
	}
	name := in.nameOr("")
	value, err := editedTemplate(name, in.configurationInput, nil)
	if err != nil {
		inputError(w, err)
		return
	}
	a.saveNewTemplate(w, req, name, value)
}

func (a *api) updateTemplate(w http.ResponseWriter, req *http.Request) {
	var in templateInput
	if err := decodeJSON(w, req, &in); err != nil {
		inputError(w, err)
		return
	}
	name := req.PathValue("name")
	a.saveEditedTemplate(w, req, name, in)
}

func (a *api) saveEditedTemplate(w http.ResponseWriter, req *http.Request, name string, in templateInput) {
	var original []byte
	if in.InlineYAML == nil {
		saved, err := a.db.ManifestTemplate(req.Context(), name)
		if err != nil {
			recordError(w, req, err, "cannot read template")
			return
		}
		original = saved.Content
		if in.Environment == nil {
			in.Environment = environmentInput(saved.Environment)
		}
	}
	newName := in.nameOr(name)
	value, err := editedTemplate(newName, in.configurationInput, original)
	if err != nil {
		inputError(w, err)
		return
	}
	if err = a.db.UpdateManifestTemplate(req.Context(), name, newName, value); err != nil {
		templateUpdateError(w, req, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func templateUpdateError(w http.ResponseWriter, req *http.Request, err error) {
	if errors.Is(err, ledger.ErrTemplateNameTaken) {
		http.Error(w, duplicateTemplateName, http.StatusConflict)
		return
	}
	recordError(w, req, err, "cannot save template")
}

func editedTemplate(name string, in configurationInput, original []byte) (ledger.Template, error) {
	if !validTemplateName(name) {
		return ledger.Template{}, errors.New("enter a template name up to 255 characters without slashes or control characters")
	}
	if len(in.References) != 0 {
		return ledger.Template{}, errors.New("set template service defaults in the manifest")
	}
	source, env := in.manifest(original), ledger.Environment(in.Environment)
	if err := validateConfiguration(source, env); err != nil {
		return ledger.Template{}, err
	}
	return ledger.Template{Content: source.Content, Environment: env}, nil
}

func recordError(w http.ResponseWriter, req *http.Request, err error, message string) {
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
	} else {
		http.Error(w, message, http.StatusInternalServerError)
	}
}
