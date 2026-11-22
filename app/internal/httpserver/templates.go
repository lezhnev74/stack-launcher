package httpserver

import (
	"database/sql"
	"errors"
	"io"
	"maps"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/manifest"
)

const maxTemplateSize = 16 << 20
const duplicateTemplateName = "a template with this name already exists"

func validTemplateName(name string) bool {
	switch strings.TrimSpace(name) {
	case "", ".", "..":
		return false
	}
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= 255 &&
		!strings.ContainsAny(name, `/\`) && strings.IndexFunc(name, unicode.IsControl) == -1
}

func validUploadFilename(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return (ext == ".yaml" || ext == ".yml") && len(name) <= 255 && validTemplateName(name)
}

func serveManifestExample(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="compose.yaml"`)
	w.Header().Set("Content-Type", "application/yaml")
	http.ServeFileFS(w, req, webTemplates, "static/manifest-example.yaml")
}

func (a *api) uploadTemplate(w http.ResponseWriter, req *http.Request) {
	name, content, err := readTemplateUpload(w, req)
	if err != nil {
		inputError(w, err)
		return
	}
	if err = compose.ValidateTemplate(content); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.saveNewTemplate(w, req, name, ledger.Template{Content: content})
}

func (a *api) saveNewTemplate(w http.ResponseWriter, req *http.Request, name string, value ledger.Template) {
	added, err := a.db.AddManifestTemplate(req.Context(), name, value)
	if err != nil {
		http.Error(w, "cannot save manifest template", http.StatusInternalServerError)
		return
	}
	if !added {
		http.Error(w, duplicateTemplateName, http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": name})
}

func readTemplateUpload(w http.ResponseWriter, req *http.Request) (string, []byte, error) {
	req.Body = http.MaxBytesReader(w, req.Body, maxBody)
	if err := req.ParseMultipartForm(maxTemplateSize); err != nil { // #nosec G120 -- MaxBytesReader bounds the entire multipart body.
		return "", nil, err
	}
	defer func() { _ = req.MultipartForm.RemoveAll() }()
	file, header, err := req.FormFile("manifest")
	if err != nil {
		return "", nil, errors.New("select a Compose YAML file to upload")
	}
	defer func() { _ = file.Close() }()
	if !validUploadFilename(header.Filename) {
		return "", nil, errors.New("use a filename ending in .yaml or .yml")
	}
	if header.Size > maxTemplateSize {
		return "", nil, &http.MaxBytesError{Limit: maxTemplateSize}
	}
	content, err := io.ReadAll(file)
	return header.Filename, content, err
}

func (a *api) launchSource(w http.ResponseWriter, req *http.Request, in *launchInput) (manifest.Input, bool) {
	switch {
	case in.ManifestTemplate != nil:
		value, err := a.db.ManifestTemplate(req.Context(), *in.ManifestTemplate)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "manifest template not found", http.StatusNotFound)
			return manifest.Input{}, false
		}
		if err != nil {
			http.Error(w, "cannot read manifest template", http.StatusInternalServerError)
			return manifest.Input{}, false
		}
		in.Environment = environmentInput(mergeTemplateEnvironment(value.Environment, ledger.Environment(in.Environment)))
		return manifest.Input{InlineYAML: value.Content}, true
	case in.Reference != nil:
		return manifest.Input{Reference: *in.Reference}, true
	default:
		return manifest.Input{InlineYAML: []byte(*in.InlineYAML)}, true
	}
}

func mergeTemplateEnvironment(defaults, overrides ledger.Environment) ledger.Environment {
	for service, values := range overrides {
		if current, ok := defaults[service]; ok {
			maps.Copy(current, values)
		} else {
			defaults[service] = values
		}
	}
	return defaults
}
