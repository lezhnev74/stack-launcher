package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"stack-launcher.local/app/internal/httpserver"
)

func TestHandler(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"GET liveness", http.MethodGet, "/healthz", http.StatusNoContent},
		{"HEAD liveness", http.MethodHead, "/healthz", http.StatusNoContent},
		{"wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{"unknown route", http.MethodGet, "/", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
			httpserver.NewHandler().ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			if tc.status == http.StatusNoContent && response.Body.Len() != 0 {
				t.Fatalf("liveness response body = %q, want empty", response.Body.String())
			}
		})
	}
}
