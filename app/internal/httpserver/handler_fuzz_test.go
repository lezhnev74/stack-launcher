package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stack-launcher.local/app/internal/httpserver"
)

func FuzzHealthz(f *testing.F) {
	f.Add(http.MethodGet, "/healthz")
	f.Add(http.MethodHead, "/healthz")
	f.Add(http.MethodPost, "/healthz")
	f.Add(http.MethodGet, "/healthz?probe=1")
	f.Add(http.MethodGet, "/%68ealthz")
	f.Add(http.MethodGet, "/./healthz")
	f.Add(http.MethodGet, "/unknown")

	handler := httpserver.NewHandler()
	f.Fuzz(func(t *testing.T, method, target string) {
		// Exercise origin-form targets that the HTTP request constructor accepts.
		if method == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
			return
		}
		request, err := http.NewRequestWithContext(t.Context(), method, target, nil)
		if err != nil {
			return
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if request.URL.Path != "/healthz" {
			return
		}

		want := http.StatusMethodNotAllowed
		if method == http.MethodGet || method == http.MethodHead {
			want = http.StatusNoContent
		}
		if response.Code != want {
			t.Fatalf("status = %d, want %d", response.Code, want)
		}
		if want == http.StatusNoContent && response.Body.Len() != 0 {
			t.Fatalf("liveness response body = %q, want empty", response.Body.String())
		}
	})
}
