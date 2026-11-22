package httpserver

import "net/http"

func NewHandler() http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /healthz", health)
	return router
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
