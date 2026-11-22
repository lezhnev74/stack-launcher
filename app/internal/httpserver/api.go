package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"stack-launcher.local/app/internal/caddy"
	"stack-launcher.local/app/internal/compose"
	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
	"stack-launcher.local/app/internal/manifest"
	"stack-launcher.local/app/internal/podman"
	"stack-launcher.local/app/internal/runlog"
)

const maxBody = 17 << 20
const acquisitionTimeout = 2 * time.Minute
const recordTimeout = 10 * time.Second
const retryInterval = time.Second
const operationConflict = "another lifecycle operation is already in progress for this stack"
const defaultDuration = 168 * time.Hour

var errJSONContentType = errors.New("Content-Type must be application/json")

type router interface {
	Publish(context.Context, string, netip.AddrPort, bool) error
	Abort(context.Context, string) error
	Withdraw(context.Context, string) error
}
type api struct {
	ctx            context.Context
	cfg            config.Config
	db             *ledger.Ledger
	router         router
	disabledReason string
	acquire        func(context.Context, config.Config, *ledger.Ledger, string, manifest.Input) (ledger.Manifest, error)
	operations     sync.Map
	stop           func(context.Context, config.Config, *ledger.Ledger, string) error
	remove         func(context.Context, string) error
	start          func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error)
	retryPinned    func(context.Context, config.Config, *ledger.Ledger, string) (podman.Result, error)
	launch         func(context.Context, config.Config, *ledger.Ledger, string, map[string]string) (podman.Result, error)
}
type launchInput struct {
	Reference        *string           `json:"reference"`
	InlineYAML       *string           `json:"inline_yaml"`
	ManifestTemplate *string           `json:"manifest_template"`
	Duration         *string           `json:"duration"`
	Images           map[string]string `json:"images"`
	Environment      environmentInput  `json:"environment"`
}

// NewAPI builds the stack API handler after running recovery.
func NewAPI(ctx context.Context, cfg config.Config, db *ledger.Ledger) (http.Handler, error) {
	a := &api{ctx: ctx, cfg: cfg, db: db, acquire: acquireManifest, launch: podman.Launch, start: podman.Start, retryPinned: podman.RetryPinned, stop: podman.Stop, remove: podman.Remove}
	if cfg.BaseDomain == "" {
		a.disabledReason = "Caddy ingress is required to publish run URLs. Set base_domain and caddy_admin_socket in the launcher configuration."
	} else {
		r, err := caddy.New(cfg.BaseDomain, cfg.CaddyAdminSocket)
		if err != nil {
			return nil, err
		}
		a.router = r
	}
	if err := a.recover(ctx); err != nil {
		return nil, err
	}
	if a.router != nil {
		go a.retryWithdrawals(ctx)
		go a.expiryLoop(ctx)
	}
	return a.handler(), nil
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /ui/controller.js", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFileFS(w, req, webTemplates, "static/controller.js")
	})
	mux.HandleFunc("GET /ui/environment-editor.js", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFileFS(w, req, webTemplates, "static/environment-editor.js")
	})
	mux.HandleFunc("GET /ui/manifest-example.yaml", serveManifestExample)
	mux.HandleFunc("POST /ui/manifest-templates", a.uploadTemplate)
	mux.HandleFunc("POST /ui/manifest-services", inspectServices)
	mux.HandleFunc("GET /ui/templates", a.webTemplates)
	mux.HandleFunc("GET /ui/templates/edit", a.webTemplate)
	mux.HandleFunc("POST /ui/templates", a.createTemplate)
	mux.HandleFunc("PUT /ui/templates/{name}", a.updateTemplate)
	mux.HandleFunc("POST /ui/runs", a.createDraft)
	mux.HandleFunc("PUT /ui/runs/{id}", a.updateDraft)
	mux.HandleFunc("POST /ui/runs/{id}/launch", a.launchDraft)
	mux.HandleFunc("GET /ui", a.webList)
	mux.HandleFunc("GET /ui/stacks", a.webDetail)
	mux.HandleFunc("GET /ui/stacks/activity", a.webLaunchActivity)
	mux.HandleFunc("GET /ui/stacks/manifest", a.webRunManifest)
	mux.HandleFunc("POST /stacks", a.post)
	mux.HandleFunc("GET /stacks/{id}", a.get)
	mux.HandleFunc("DELETE /stacks/{id}", a.removeStack)
	mux.HandleFunc("GET /stacks/{id}/environment", a.getEnvironment)
	mux.HandleFunc("PUT /stacks/{id}/environment", a.putEnvironment)
	mux.HandleFunc("POST /stacks/{id}/stop", a.stopStack)
	mux.HandleFunc("POST /stacks/{id}/start", a.startStack)
	mux.HandleFunc("POST /stacks/{id}/retry", a.retryStack)
	return http.NewCrossOriginProtection().Handler(mux)
}
func (a *api) post(w http.ResponseWriter, req *http.Request) {
	if a.disabledReason != "" {
		http.Error(w, a.disabledReason, http.StatusServiceUnavailable)
		return
	}
	input, duration, err := decodeInput(w, req)
	if err != nil {
		inputError(w, err)
		return
	}
	source, ok := a.launchSource(w, req, &input)
	if !ok {
		return
	}
	var template string
	if input.ManifestTemplate != nil {
		template = *input.ManifestTemplate
	}
	request := ledger.LaunchRequest{Reference: source.Reference, InlineYAML: source.InlineYAML, Images: input.Images}
	id, host, err := a.reserveLaunch(req.Context(), time.Now(), duration, ledger.Environment(input.Environment), template, &request)
	if err != nil {
		launchReservationError(w, err)
		return
	}
	runlog.Printf(id, "Launch accepted source=api hostname=%q duration=%s", host, duration)
	go a.work(id, host, source, input.Images, duration) // #nosec G118 -- accepted work outlives the HTTP request
	acceptLaunch(w, id)
}

func launchReservationError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBaseDomainLength) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "cannot create stack", http.StatusInternalServerError)
}

func acceptLaunch(w http.ResponseWriter, id string) {
	w.Header().Set("Location", "/stacks/"+id)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

func inputError(w http.ResponseWriter, err error) {
	var large *http.MaxBytesError
	code := http.StatusBadRequest
	switch {
	case errors.As(err, &large):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(err, errJSONContentType):
		code = http.StatusUnsupportedMediaType
	}
	http.Error(w, err.Error(), code)
}
func decodeInput(w http.ResponseWriter, req *http.Request) (launchInput, time.Duration, error) {
	var in launchInput
	if err := decodeJSON(w, req, &in); err != nil {
		return in, 0, err
	}
	duration, err := validateInput(in)
	return in, duration, err
}
func decodeJSON(w http.ResponseWriter, req *http.Request, target any) error {
	if !hasJSONContentType(req.Header) {
		return errJSONContentType
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("expected one JSON object")
		}
		return err
	}
	return nil
}
func hasJSONContentType(headers http.Header) bool {
	values := headers.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	kind, _, err := mime.ParseMediaType(values[0])
	return err == nil && kind == "application/json"
}
func validateInput(in launchInput) (time.Duration, error) {
	if !validSource(in) {
		return 0, errors.New("provide exactly one of reference, inline_yaml, or manifest_template")
	}
	duration := defaultDuration
	if in.Duration != nil {
		var err error
		duration, err = time.ParseDuration(*in.Duration)
		if err != nil || duration <= 0 {
			return 0, errors.New("duration must be a positive Go duration")
		}
	}
	if err := compose.ValidateEnvironment(ledger.Environment(in.Environment)); err != nil {
		return 0, err
	}
	if err := validateImages(in.Images); err != nil {
		return 0, err
	}
	return duration, nil
}
func validSource(in launchInput) bool {
	if in.ManifestTemplate != nil {
		return in.Reference == nil && in.InlineYAML == nil && validTemplateName(*in.ManifestTemplate)
	}
	return (in.Reference == nil) != (in.InlineYAML == nil) && (in.Reference == nil || strings.TrimSpace(*in.Reference) != "")
}
func validateImages(images map[string]string) error {
	for service, ref := range images {
		if service == "" || strings.TrimSpace(service) != service || !validImage(ref) {
			return errors.New("invalid service image override")
		}
	}
	return nil
}
func validImage(ref string) bool {
	return ref != "" && strings.TrimSpace(ref) == ref && !strings.ContainsAny(ref, " \t\r\n") && !strings.Contains(ref, "://")
}

func (a *api) get(w http.ResponseWriter, req *http.Request) {
	status, err := a.db.Status(req.Context(), req.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, "cannot read stack", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func (a *api) work(id, host string, source manifest.Input, images map[string]string, duration time.Duration) {
	runlog.Printf(id, "Acquiring manifest timeout=%s", acquisitionTimeout)
	a.launchNote(id, "Acquiring the original Compose manifest.")
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), acquisitionTimeout)
	saved, err := a.acquire(ctx, a.cfg, a.db, id, source)
	cancel()
	if err != nil {
		a.fail(id, host, "acquisition", err, false)
		a.operations.Delete(id)
		return
	}
	runlog.Printf(id, "Manifest saved elapsed=%s git_commit=%q", time.Since(started), saved.Commit)
	a.launchWork(id, host, images, duration)
}

func (a *api) launchWork(id, host string, images map[string]string, duration time.Duration) {
	defer a.operations.Delete(id)
	a.launchNote(id, "Preparing the saved Compose manifest and environment.")
	result, err := a.launch(a.launchContext(id), a.cfg, a.db, id, images)
	a.completeLaunch(id, host, duration, result, err)
}

func (a *api) completeLaunch(id, host string, duration time.Duration, result podman.Result, err error) {
	if err != nil {
		a.fail(id, host, "startup", err, false)
		return
	}
	a.publish(id, host, result, duration)
}
func (a *api) publish(id, host string, result podman.Result, duration time.Duration) {
	started := time.Now()
	runlog.Printf(id, "Recording pending Caddy publication hostname=%q", host)
	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	err := a.db.Pending(ctx, id)
	cancel()
	if err != nil {
		a.fail(id, host, "publication", err, false)
		return
	}
	a.launchNote(id, "Publishing the app URL through Caddy.")
	runlog.Printf(id, "Publishing Caddy route hostname=%q upstream=%s https=%t", host, result.Endpoint, result.HTTPS)
	if err = a.router.Publish(context.Background(), host, result.Endpoint, result.HTTPS); err != nil {
		a.fail(id, host, "publication", err, true)
		return
	}
	a.launchNote(id, "App URL published.")
	runlog.Printf(id, "Caddy route published elapsed=%s, recording ready state", time.Since(started))
	scheme := "http"
	if result.HTTPS {
		scheme = "https"
	}
	ctx, cancel = context.WithTimeout(context.Background(), recordTimeout)
	err = a.db.Ready(ctx, id, scheme+"://"+host, result.Bindings, time.Now(), duration)
	cancel()
	if err != nil {
		a.fail(id, host, "ready", err, true)
		return
	}
	runlog.Printf(id, "Run ready url=%q", scheme+"://"+host)
}
func (a *api) fail(id, host, phase string, cause error, pending bool) {
	message := cause.Error()
	runlog.Printf(id, "Run failed phase=%s publication_pending=%t error=%q", phase, pending, message)
	a.launchNote(id, "Run failed in the "+phase+" phase.")
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		done, stillPending := a.storeFailure(id, host, phase, message, pending)
		if done {
			return
		}
		pending = stillPending
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *api) storeFailure(id, host, phase, message string, needsFence bool) (done, stillPending bool) {
	ctx, cancel := context.WithTimeout(a.ctx, recordTimeout)
	defer cancel()
	changed, err := a.db.Fail(ctx, id, phase, message)
	if err != nil {
		runlog.Printf(id, "Could not record run failure, will retry error=%q", err)
		return false, needsFence && !a.fencePending(id, host)
	}
	if changed {
		runlog.Printf(id, "Run failure recorded phase=%s", phase)
		a.finishWithdrawal(id, host, needsFence)
	}
	return true, false
}

func (a *api) fencePending(id, host string) bool {
	ctx, cancel := context.WithTimeout(a.ctx, recordTimeout)
	defer cancel()
	status, err := a.db.Status(ctx, id)
	return err == nil && status.Pending && (status.State == "launching" || status.State == "starting") && a.fence(ctx, id, host)
}

func (a *api) finishWithdrawal(id, host string, needsFence bool) {
	ctx, cancel := context.WithTimeout(a.ctx, recordTimeout)
	defer cancel()
	if needsFence {
		a.abort(ctx, id, host)
		return
	}
	a.clearPending(ctx, id)
}

func (a *api) fence(ctx context.Context, id, host string) bool {
	runlog.Printf(id, "Aborting Caddy publication hostname=%q", host)
	if err := a.router.Abort(ctx, host); err != nil {
		runlog.Printf(id, "Could not abort Caddy publication error=%q", err)
		return false
	}
	runlog.Printf(id, "Caddy publication aborted hostname=%q", host)
	return true
}

func (a *api) abort(ctx context.Context, id, host string) {
	if a.fence(ctx, id, host) {
		a.clearPending(ctx, id)
	}
}

func (a *api) clearPending(ctx context.Context, id string) {
	if err := a.db.ClearPending(ctx, id); err != nil {
		runlog.Printf(id, "Could not clear publication marker error=%q", err)
	}
}

func acquireManifest(ctx context.Context, cfg config.Config, db *ledger.Ledger, id string, input manifest.Input) (ledger.Manifest, error) {
	return manifest.Acquire(ctx, cfg, db, id, input)
}
