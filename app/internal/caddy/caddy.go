// Package caddy manages stack routes through Caddy's Unix admin socket.
package caddy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

const routesPath = "/config/apps/http/servers/stack-launcher/routes"
const requestTimeout = 5 * time.Second
const retries = 5
const maxRoutesBytes = 4 << 20

// Router updates the dedicated Caddy server's routes.
type Router struct {
	base   string
	client *http.Client
}

// ValidDomain accepts ASCII DNS names without a trailing dot.
func ValidDomain(name string) bool {
	if len(name) == 0 || len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}
func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-", c) {
			return false
		}
	}
	return true
}

// New creates a router without connecting to the admin socket.
func New(baseDomain, socket string) (*Router, error) {
	if !ValidDomain(baseDomain) {
		return nil, errors.New("invalid base_domain")
	}
	if socket == "" || strings.Contains(socket, "://") {
		return nil, errors.New("invalid caddy_admin_socket")
	}
	transport := &http.Transport{Proxy: nil, IdleConnTimeout: requestTimeout, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{
		Transport: transport,
		Timeout:   requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Router{base: strings.ToLower(baseDomain), client: client}, nil
}
func (r *Router) hostname(host string) (string, error) {
	if !ValidDomain(host) {
		return "", errors.New("invalid hostname")
	}
	host = strings.ToLower(host)
	label, ok := strings.CutSuffix(host, "."+r.base)
	if !ok || strings.Contains(label, ".") {
		return "", errors.New("hostname must be one DNS label under base_domain")
	}
	return host, nil
}
func routeID(host string) string {
	sum := sha256.Sum256([]byte(host))
	return "stack-launcher-" + hex.EncodeToString(sum[:])
}

// Publish atomically adds or replaces a hostname route.
func (r *Router) Publish(ctx context.Context, hostname string, backend netip.AddrPort, https bool) error {
	host, err := r.hostname(hostname)
	if err != nil {
		return err
	}
	if backend.Addr() != netip.MustParseAddr("127.0.0.1") || backend.Port() == 0 {
		return errors.New("backend must be 127.0.0.1 with a nonzero port")
	}
	route, err := makeRoute(host, backend.String(), https)
	if err != nil {
		return err
	}
	return r.update(ctx, routeID(host), route)
}

// Withdraw atomically removes a hostname route, if present.
func (r *Router) Withdraw(ctx context.Context, hostname string) error {
	host, err := r.hostname(hostname)
	if err != nil {
		return err
	}
	return r.update(ctx, routeID(host), nil)
}

// Abort fences delayed publication and removes an incomplete route atomically.
// The inert revision changes the routes ETag even when the hostname was absent.
func (r *Router) Abort(ctx context.Context, hostname string) error {
	host, err := r.hostname(hostname)
	if err != nil {
		return err
	}
	marker, err := revisionMarker()
	if err != nil {
		return err
	}
	for range retries {
		status, err := r.abortPatch(ctx, routeID(host), marker)
		if err != nil {
			return err
		}
		if status != http.StatusPreconditionFailed {
			return patchStatus(status)
		}
	}
	return errors.New("caddy routes changed repeatedly")
}
func revisionMarker() (json.RawMessage, error) {
	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"@id": "stack-launcher-revision-" + hex.EncodeToString(revision[:])})
}
func (r *Router) abortPatch(ctx context.Context, id string, marker json.RawMessage) (int, error) {
	routes, tag, err := r.read(ctx)
	if err != nil {
		return 0, err
	}
	routes, _, err = change(routes, id, nil)
	if err != nil {
		return 0, err
	}
	routes, err = replaceRevision(routes, marker)
	if err != nil {
		return 0, err
	}
	return r.patch(ctx, tag, routes)
}

func replaceRevision(routes []json.RawMessage, marker json.RawMessage) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, 0, len(routes)+1)
	for _, route := range routes {
		var entry struct {
			ID any `json:"@id"`
		}
		if err := json.Unmarshal(route, &entry); err != nil {
			return nil, err
		}
		id, _ := entry.ID.(string)
		if !strings.HasPrefix(id, "stack-launcher-revision-") {
			result = append(result, route)
		}
	}
	return append(result, marker), nil
}

func makeRoute(host, backend string, https bool) (json.RawMessage, error) {
	proxy := map[string]any{"handler": "reverse_proxy", "upstreams": []map[string]string{{"dial": backend}}}
	httpHandler := proxy
	tlsHandler := map[string]any{"handler": "static_response", "status_code": http.StatusNotFound}
	if https {
		httpHandler = map[string]any{
			"handler":     "static_response",
			"status_code": http.StatusPermanentRedirect,
			"headers":     map[string][]string{"Location": {"https://" + host + "{http.request.uri}"}},
		}
		tlsHandler = proxy
	}
	route := map[string]any{
		"@id":      routeID(host),
		"match":    []map[string][]string{{"host": {host}}},
		"terminal": true,
		"handle": []map[string]any{{
			"handler": "subroute",
			"routes": []map[string]any{
				{"match": []map[string]string{{"protocol": "https"}}, "handle": []map[string]any{tlsHandler}},
				{"match": []map[string]string{{"protocol": "http"}}, "handle": []map[string]any{httpHandler}},
			},
		}},
	}
	return json.Marshal(route)
}
func (r *Router) update(ctx context.Context, id string, desired json.RawMessage) error {
	for range retries {
		routes, tag, err := r.read(ctx)
		if err != nil {
			return err
		}
		next, changed, err := change(routes, id, desired)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		status, err := r.patch(ctx, tag, next)
		if err != nil {
			return err
		}
		if status != http.StatusPreconditionFailed {
			return patchStatus(status)
		}
	}
	return errors.New("caddy routes changed repeatedly")
}
func patchStatus(status int) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("caddy PATCH routes: HTTP %d (check Caddy logs)", status)
	}
	return nil
}
func change(routes []json.RawMessage, id string, desired json.RawMessage) ([]json.RawMessage, bool, error) {
	for i, raw := range routes {
		var entry struct {
			ID any `json:"@id"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, false, fmt.Errorf("invalid caddy route: %w", err)
		}
		entryID, _ := entry.ID.(string)
		if entryID == id {
			if desired == nil {
				return append(routes[:i], routes[i+1:]...), true, nil
			}
			if jsonEqual(raw, desired) {
				return routes, false, nil
			}
			routes[i] = desired
			return routes, true, nil
		}
	}
	if desired == nil {
		return routes, false, nil
	}
	return append([]json.RawMessage{desired}, routes...), true, nil
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}
func (r *Router) read(ctx context.Context) ([]json.RawMessage, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://caddy"+routesPath, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("caddy GET routes: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("caddy GET routes: HTTP %d", resp.StatusCode)
	}
	tag := resp.Header.Get("ETag")
	if tag == "" {
		return nil, "", errors.New("caddy GET routes: missing ETag")
	}
	routes, err := readRoutes(resp.Body)
	return routes, tag, err
}

func readRoutes(body io.Reader) ([]json.RawMessage, error) {
	payload, err := io.ReadAll(io.LimitReader(body, maxRoutesBytes+1))
	if err != nil {
		return nil, fmt.Errorf("caddy GET routes: read response: %w", err)
	}
	if len(payload) > maxRoutesBytes {
		return nil, errors.New("caddy GET routes: response exceeds 4 MiB")
	}
	var routes []json.RawMessage
	if err := json.Unmarshal(payload, &routes); err != nil || routes == nil {
		return nil, errors.New("caddy GET routes: invalid route array")
	}
	return routes, nil
}
func (r *Router) patch(ctx context.Context, tag string, routes []json.RawMessage) (int, error) {
	body, err := json.Marshal(routes)
	if err != nil {
		return 0, err
	}
	// Leave room for the recovery revision and Caddy's response newline.
	if len(body) > maxRoutesBytes-128 {
		return 0, errors.New("caddy PATCH routes: configuration exceeds the update size limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "http://caddy"+routesPath, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("If-Match", tag)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("caddy PATCH routes: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
