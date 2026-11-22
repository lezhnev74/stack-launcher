package caddy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRealCaddy(t *testing.T) {
	if os.Getenv("STACK_LAUNCHER_CADDY_TEST") != "1" {
		t.Skip("set STACK_LAUNCHER_CADDY_TEST=1")
	}
	binary, err := exec.LookPath("caddy")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "admin.sock")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "*.example.test"}, DNSNames: []string{"*.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
		t.Fatal(err)
	}
	listen := func() net.Listener {
		l, e := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = l.Close() })
		return l
	}
	httpListener, tlsListener := listen(), listen()
	httpAddr, tlsAddr := httpListener.Addr().String(), tlsListener.Addr().String()
	httpPort, tlsPort := httpListener.Addr().(*net.TCPAddr).Port, tlsListener.Addr().(*net.TCPAddr).Port
	config := fmt.Sprintf(`{
		"admin": {"listen": %q},
		"apps": {
			"tls": {"certificates": {"load_files": [{"certificate": %q, "key": %q}]}},
			"http": {"http_port": %d, "https_port": %d, "servers": {"stack-launcher": {
				"listen": [%q, %q],
				"protocols": ["h1", "h2"],
				"automatic_https": {"disable": true},
				"tls_connection_policies": [{}],
				"routes": [{"handle": [{"handler": "static_response", "status_code": 404}]}]
			}}}
		}
	}`, "unix/"+socket, certFile, keyFile, httpPort, tlsPort, httpAddr, tlsAddr)
	cfg := filepath.Join(dir, "caddy.json")
	if err := os.WriteFile(cfg, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), binary, "run", "--config", cfg) // #nosec G204 -- opt-in test uses the Caddy binary from PATH
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.Env = append(cmd.Environ(), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_DATA_HOME="+filepath.Join(dir, "data"))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Caddy output: %s", output.String())
		}
	})
	_ = httpListener.Close()
	_ = tlsListener.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	r, err := New("example.test", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	startup, cancelStartup := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStartup()
	for {
		_, _, err := r.read(startup)
		if err == nil {
			break
		}
		if startup.Err() != nil {
			t.Fatalf("Caddy route list did not become available: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	app := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	}
	one, two := app("one"), app("two")
	defer one.Close()
	defer two.Close()
	addr := func(s *httptest.Server) netip.AddrPort { return netip.MustParseAddrPort(s.Listener.Addr().String()) }
	if err := r.Publish(ctx, "one.example.test", addr(one), true); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(ctx, "two.example.test", addr(two), false); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(cert)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, tlsAddr)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	plain := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, httpAddr)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	check := func(c *http.Client, url string, status int, body, location string) {
		t.Helper()
		requestCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, e := http.NewRequestWithContext(requestCtx, http.MethodGet, url, nil)
		if e != nil {
			t.Fatal(e)
		}
		resp, e := c.Do(req)
		// A dial can land on a listener Caddy is closing during a config reload.
		for errors.Is(e, syscall.ECONNRESET) && requestCtx.Err() == nil {
			time.Sleep(20 * time.Millisecond)
			resp, e = c.Do(req)
		}
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = resp.Body.Close() }()
		b, e := io.ReadAll(resp.Body)
		if e != nil {
			t.Fatal(e)
		}
		if resp.StatusCode != status || string(b) != body || resp.Header.Get("Location") != location {
			t.Fatalf("%s: %d %q %q", url, resp.StatusCode, b, resp.Header.Get("Location"))
		}
	}
	// A delayed publication based on the old ETag must fail after recovery,
	// even when its route was absent at the time of the abort.
	routes, tag, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	delayed, err := makeRoute("delayed.example.test", addr(one).String(), false)
	if err != nil {
		t.Fatal(err)
	}
	routes, _, err = change(routes, routeID("delayed.example.test"), delayed)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Abort(ctx, "delayed.example.test"); err != nil {
		t.Fatal(err)
	}
	status, err := r.patch(ctx, tag, routes)
	if err != nil || status != http.StatusPreconditionFailed {
		t.Fatalf("delayed patch: %d %v", status, err)
	}
	if err = r.Abort(ctx, "delayed.example.test"); err != nil {
		t.Fatal(err)
	}
	check(client, "https://one.example.test/", 200, "one", "")
	check(plain, "http://one.example.test/a%2Fb?q=x%2Fy", 308, "", "https://one.example.test/a%2Fb?q=x%2Fy")
	check(plain, "http://two.example.test/", 200, "two", "")
	check(client, "https://two.example.test/", 404, "", "")
	if err := r.Withdraw(ctx, "one.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := r.Withdraw(ctx, "one.example.test"); err != nil {
		t.Fatal(err)
	}
	check(client, "https://one.example.test/", 404, "", "")
	check(plain, "http://one.example.test/", 404, "", "")
	check(plain, "http://two.example.test/", 200, "two", "")
	checkRestartFence(t, r, "two.example.test", addr(two))
	check(plain, "http://two.example.test/", 200, "two", "")
	if err := r.Publish(ctx, "one.example.test", addr(one), true); err != nil {
		t.Fatal(err)
	}
	if err := r.Abort(ctx, "one.example.test"); err != nil {
		t.Fatal(err)
	}
	check(client, "https://one.example.test/", 404, "", "")
	check(plain, "http://two.example.test/", 200, "two", "")
}

func checkRestartFence(t *testing.T, r *Router, host string, backend netip.AddrPort) {
	t.Helper()
	ctx := t.Context()
	routes, tag, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	withdrawal, changed, err := change(routes, routeID(host), nil)
	if err != nil || !changed {
		t.Fatalf("prepare old withdrawal: %t %v", changed, err)
	}
	if err := r.Withdraw(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := r.Abort(ctx, host); err != nil {
		t.Fatal(err)
	}
	if err := r.Publish(ctx, host, backend, false); err != nil {
		t.Fatal(err)
	}
	// Start can get the same port. An old Stop must still fail its ETag check.
	status, err := r.patch(ctx, tag, withdrawal)
	if err != nil || status != http.StatusPreconditionFailed {
		t.Fatalf("old withdrawal: %d %v", status, err)
	}
}
