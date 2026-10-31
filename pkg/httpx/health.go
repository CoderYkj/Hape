package httpx

import (
	"context"
	"net/http"
)

// Health mounts liveness, readiness and metrics endpoints.
//
// Liveness must never depend on downstream services: a slow database must not
// make the orchestrator kill a healthy process. Readiness is where dependency
// checks belong.
func (r *Router) Health(ready func(ctx context.Context) error) {
	r.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": r.service})
	})
	r.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, rq *http.Request) {
		if ready != nil {
			if err := ready(rq.Context()); err != nil {
				WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
					"status": "unavailable",
					"reason": err.Error(),
				})
				return
			}
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready", "service": r.service})
	})
	r.mux.Handle("GET /metrics", r.metrics.Handler())
}
