package httpserver

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"time"

	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/ledger"
)

//go:embed templates/*.html static/*.js static/*.yaml
var webTemplates embed.FS

var pages = template.Must(template.ParseFS(webTemplates, "templates/*.html"))

type stackList struct {
	Statuses  []ledger.Status
	Templates []string
}

type stackDetail struct {
	ledger.Status
	Ports                        map[string]map[int]string
	Services                     map[string]runService
	Activity                     []ledger.LaunchActivity
	CanStop, CanStart, CanRemove bool
}

func (s stackDetail) InProgress() bool { return s.State == "launching" || s.State == "starting" }

func (s stackDetail) ExpiryLabel() string {
	expires, err := time.Parse(time.RFC3339Nano, s.ExpiresAt)
	if err != nil {
		return s.ExpiresAt
	}
	return expires.UTC().Format("2 Jan 2006, 15:04 MST")
}

func (s stackDetail) ServiceNames() []string { return slices.Sorted(maps.Keys(s.Services)) }

type runService struct {
	Environment map[string]string
	Source      compose.ServiceReference
	Image       ledger.Image
}

type templateSummary struct {
	Name     string
	Services []string
}

type configurationPage struct {
	ID, Name, LaunchDisabled string
	Seed                     configurationSeed
}

type configurationSeed struct {
	Content     []byte                              `json:"content"`
	Services    []compose.ServiceReference          `json:"services"`
	Environment ledger.Environment                  `json:"environment"`
	References  map[string]ledger.ReferenceOverride `json:"references"`
	Problem     string                              `json:"problem"`
}

func renderWeb(w http.ResponseWriter, name string, data any) {
	var body bytes.Buffer
	if err := pages.ExecuteTemplate(&body, name, data); err != nil {
		http.Error(w, "cannot render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body.Bytes())
}

func (a *api) webList(w http.ResponseWriter, req *http.Request) {
	statuses, err := a.db.Statuses(req.Context())
	if err != nil {
		http.Error(w, "cannot read stacks", http.StatusInternalServerError)
		return
	}
	templates, err := a.db.ManifestTemplates(req.Context())
	if err != nil {
		http.Error(w, "cannot read manifest templates", http.StatusInternalServerError)
		return
	}
	renderWeb(w, "list.html", stackList{Statuses: statuses, Templates: templates})
}

func (a *api) webDetail(w http.ResponseWriter, req *http.Request) {
	s, err := a.db.Status(req.Context(), req.URL.Query().Get("id"))
	if err != nil {
		recordError(w, req, err, "cannot read run")
		return
	}
	if s.State == "draft" {
		a.webDraft(w, req, s)
		return
	}
	a.webRun(w, req, s)
}

func (a *api) webRun(w http.ResponseWriter, req *http.Request, s ledger.Status) {
	detail := makeStackDetail(s)
	activity, err := a.db.LaunchHistory(req.Context(), s.ID)
	if err != nil {
		http.Error(w, "cannot read launch history", http.StatusInternalServerError)
		return
	}
	detail.Activity = activity
	if err := a.loadRunConfiguration(req.Context(), &detail); err != nil {
		recordError(w, req, err, "cannot read run configuration")
		return
	}
	if len(s.Bindings) > 0 {
		if err := json.Unmarshal(s.Bindings, &detail.Ports); err != nil {
			http.Error(w, "cannot read stack bindings", http.StatusInternalServerError)
			return
		}
	}
	renderWeb(w, "detail.html", detail)
}

func (a *api) webTemplates(w http.ResponseWriter, req *http.Request) {
	var templates []templateSummary
	err := a.db.VisitTemplateManifests(req.Context(), func(name string, content []byte) {
		services, _ := compose.ServiceNames(ledger.Manifest{Content: content})
		templates = append(templates, templateSummary{Name: name, Services: services})
	})
	if err != nil {
		http.Error(w, "cannot read templates", http.StatusInternalServerError)
		return
	}
	renderWeb(w, "templates.html", templates)
}

func (a *api) webTemplate(w http.ResponseWriter, req *http.Request) {
	name := req.URL.Query().Get("name")
	if name == "" {
		starter, _ := webTemplates.ReadFile("static/manifest-example.yaml")
		renderWeb(w, "configure.html", configurationPage{Seed: makeSeed(ledger.Manifest{Content: starter}, nil)})
		return
	}
	value, err := a.db.ManifestTemplate(req.Context(), name)
	if err != nil {
		recordError(w, req, err, "cannot read template")
		return
	}
	renderWeb(w, "configure.html", configurationPage{Name: name, Seed: makeSeed(ledger.Manifest{Content: value.Content}, value.Environment)})
}

func (a *api) webDraft(w http.ResponseWriter, req *http.Request, status ledger.Status) {
	source, err := a.db.GetManifest(req.Context(), status.ID)
	if err != nil {
		recordError(w, req, err, "cannot read run manifest")
		return
	}
	env, err := a.db.GetEnvironment(req.Context(), status.ID)
	if err != nil {
		recordError(w, req, err, "cannot read run environment")
		return
	}
	renderWeb(w, "configure.html", configurationPage{
		ID: status.ID, Name: status.TemplateName, LaunchDisabled: a.disabledReason, Seed: makeSeed(source, env),
	})
}

func makeSeed(source ledger.Manifest, env ledger.Environment) configurationSeed {
	refs, err := inspectManifest(ledger.Manifest{Content: source.Content})
	value := configurationSeed{Content: source.Content, Services: refs, Environment: env, References: source.References}
	if err != nil {
		value.Problem = err.Error()
	}
	return value
}

func (a *api) loadRunConfiguration(ctx context.Context, detail *stackDetail) error {
	env, err := a.db.GetEnvironment(ctx, detail.ID)
	if err != nil {
		return err
	}
	detail.Services = make(map[string]runService)
	for name, values := range env {
		detail.Services[name] = runService{Environment: values}
	}
	if err := a.loadRunManifest(ctx, detail); err != nil {
		return err
	}
	return a.loadRunImages(ctx, detail)
}

func (a *api) loadRunManifest(ctx context.Context, detail *stackDetail) error {
	if !detail.HasManifest {
		return nil
	}
	source, err := a.db.GetManifest(ctx, detail.ID)
	if err != nil {
		return err
	}
	refs, err := compose.TemplateServices(source)
	if err != nil {
		// Show any readable service names alongside saved overrides for failed runs.
		names, _ := compose.ServiceNames(source)
		for _, name := range names {
			refs = append(refs, compose.ServiceReference{Name: name})
		}
	}
	for _, ref := range refs {
		service := detail.Services[ref.Name]
		service.Source = ref
		detail.Services[ref.Name] = service
	}
	return nil
}

func (a *api) loadRunImages(ctx context.Context, detail *stackDetail) error {
	if !detail.HasImages {
		return nil
	}
	images, err := a.db.GetImages(ctx, detail.ID)
	if err != nil {
		return err
	}
	for name, image := range images {
		service := detail.Services[name]
		service.Image = image
		detail.Services[name] = service
	}
	return nil
}

func makeStackDetail(s ledger.Status) stackDetail {
	detail := stackDetail{Status: s}
	if s.APIOwned {
		detail.CanStop = s.State == "ready" || s.State == "stopping"
		detail.CanStart = s.State == "stopped"
		detail.CanRemove = s.State == "ready" || s.State == "stopping" || s.State == "stopped" || s.State == "failed" || s.State == "removing"
	}
	return detail
}
