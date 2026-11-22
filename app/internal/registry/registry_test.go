package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`

func TestValidateRepository(t *testing.T) {
	for _, name := range []string{
		"registry.example.test/team/web", "docker.io/alpine", "registry:5000/web", "localhost/web", "127.0.0.1:5000/web",
	} {
		if err := ValidateRepository(name); err != nil {
			t.Errorf("ValidateRepository(%q): %v", name, err)
		}
	}
	for _, name := range []string{
		"", "ubuntu", "team/web", "https://registry.example.test/team/web", "registry.test/team/web:latest",
		"registry.test/team/web@sha256:" + strings.Repeat("a", 64), "registry.test:65536/web", "registry.test:0/web",
		"[::1]:5000/web", "registry.test/team/Web", " registry.test/web", "registry.test/$$web",
	} {
		if err := ValidateRepository(name); err == nil {
			t.Errorf("ValidateRepository(%q) accepted an invalid repository", name)
		}
	}
}

func TestResolveDigest(t *testing.T) {
	type response struct {
		status int
		body   string
		miss   bool
	}
	cases := map[string]response{
		"present": {http.StatusOK, fixtureManifest, false},
		"missing-manifest": {http.StatusNotFound,
			`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`, true},
		"missing-repository": {http.StatusNotFound,
			`{"errors":[{"code":"NAME_UNKNOWN","message":"repository unknown"}]}`, true},
		"unauthorized": {http.StatusUnauthorized,
			`{"errors":[{"code":"UNAUTHORIZED","message":"credentials required"}]}`, false},
		"forbidden": {http.StatusForbidden,
			`{"errors":[{"code":"DENIED","message":"permission denied"}]}`, false},
		"denied-not-found": {http.StatusNotFound,
			`{"errors":[{"code":"DENIED","message":"permission denied"}]}`, false},
		"opaque-not-found": {http.StatusNotFound, "proxy route not found", false},
		"unreachable":      {http.StatusBadGateway, "upstream unavailable", false},
		"bad-document":     {http.StatusOK, "not an image manifest", false},
	}
	repository := testRegistry(t, true, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		tag, matching := strings.CutPrefix(r.URL.Path, "/v2/team/web/manifests/")
		response, found := cases[tag]
		if r.Method != http.MethodGet || !matching || !found {
			t.Errorf("unexpected registry request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(response.status)
		_, _ = fmt.Fprint(w, response.body)
	})
	for tag, response := range cases {
		t.Run(tag, func(t *testing.T) {
			value, err := ResolveDigest(t.Context(), repository, tag, "")
			if tag == "present" {
				want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(fixtureManifest)))
				if err != nil || value != want {
					t.Fatalf("digest = %q, err = %v, want %q", value, err, want)
				}
				return
			}
			if value != "" || err == nil || errors.Is(err, ErrNotFound) != response.miss {
				t.Fatalf("digest = %q, err = %v, want failure with miss = %t", value, err, response.miss)
			}
		})
	}
}

func TestResolveDigestCredentials(t *testing.T) {
	const username = "launcher"
	password := rand.Text()
	repository := testRegistry(t, true, func(w http.ResponseWriter, r *http.Request) {
		user, secret, ok := r.BasicAuth()
		if r.URL.Path == "/v2/" || !ok || user != username || secret != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, fixtureManifest)
	})
	domain, _, _ := strings.Cut(repository, "/")
	credential := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	file := fmt.Appendf(nil, `{"auths":{%q:{"auth":%q}}}`, domain, credential)
	explicitPath := filepath.Join(t.TempDir(), "explicit.json")
	writeConfigFile(t, explicitPath, file)
	t.Setenv("REGISTRY_AUTH_FILE", explicitPath)
	if _, err := ResolveDigest(t.Context(), repository, "tag", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("ambient auth override was used, or auth error classified as missing: %v", err)
	}
	if _, err := ResolveDigest(t.Context(), repository, "tag", explicitPath); err != nil {
		t.Fatalf("explicit auth file: %v", err)
	}
	defaultPath := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "containers", "auth.json")
	writeConfigFile(t, defaultPath, file)
	if _, err := ResolveDigest(t.Context(), repository, "tag", ""); err != nil {
		t.Fatalf("default auth file: %v", err)
	}
	if _, err := ResolveDigest(t.Context(), repository, "tag", explicitPath+".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit auth file: %v", err)
	}
	if _, err := ResolveDigest(t.Context(), repository, "tag", filepath.Dir(explicitPath)); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("directory supplied as auth file: %v", err)
	}
}

func TestResolveDigestUntrustedTLS(t *testing.T) {
	repository := testRegistry(t, false, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
	})
	if _, err := ResolveDigest(t.Context(), repository, "tag", ""); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("untrusted TLS: %v", err)
	}
}

func TestResolveDigestCancellation(t *testing.T) {
	repository := testRegistry(t, true, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`, http.StatusNotFound)
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ResolveDigest(ctx, repository, "tag", ""); !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotFound) {
		t.Fatalf("canceled lookup: %v", err)
	}
}

func testRegistry(t *testing.T, trusted bool, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	domain := server.Listener.Addr().String()
	configHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CONTAINERS_REGISTRIES_CONF_OVERRIDE", "")
	config := filepath.Join(configHome, "registries.conf")
	writeConfigFile(t, config, nil)
	t.Setenv("CONTAINERS_REGISTRIES_CONF", config)
	if trusted {
		ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		writeConfigFile(t, filepath.Join(configHome, "containers", "certs.d", domain, "ca.crt"), ca)
	}
	return domain + "/team/web"
}

func writeConfigFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil { // #nosec G703 -- registry test configuration belongs to a test-owned temporary directory.
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil { // #nosec G703 -- registry test configuration belongs to a test-owned temporary directory.
		t.Fatal(err)
	}
}
