package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
)

// HandlerFunc is the shape every contract handler has: read the request,
// return the response body, or an error that the router renders.
type HandlerFunc func(r *http.Request) (any, error)

// Router owns an http.ServeMux and the contract operations registered on it.
//
// One Router may host many services (the single-process platform) or exactly
// one (a split service binary). Either way the contract check in tests sees
// the same ops.
type Router struct {
	service string
	auth    Authenticator
	log     *slog.Logger
	metrics *Metrics
	mux     *http.ServeMux
	ops     []api.Op
}

// NewRouter builds a router. auth may be nil only when no authenticated op is
// registered (tests of a single public endpoint).
func NewRouter(service string, log *slog.Logger, auth Authenticator) *Router {
	if log == nil {
		log = slog.Default()
	}
	return &Router{
		service: service,
		auth:    auth,
		log:     log,
		metrics: NewMetrics(service),
		mux:     http.NewServeMux(),
	}
}

// Service is the owning service name, used in logs and metrics.
func (r *Router) Service() string { return r.service }

// Ops returns the registered contract operations, in registration order.
func (r *Router) Ops() []api.Op { return r.ops }

// Mux exposes the underlying multiplexer for non-contract endpoints.
func (r *Router) Mux() *http.ServeMux { return r.mux }

// Handle registers one contract operation. The pattern is built from Method
// and Path, so declaration and routing can never disagree.
func (r *Router) Handle(op api.Op, h HandlerFunc) {
	r.ops = append(r.ops, op)
	r.mux.Handle(op.Method+" "+op.Path, r.instrument(op, h))
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, rq *http.Request) { r.mux.ServeHTTP(w, rq) }

func (r *Router) instrument(op api.Op, h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		reqID := rq.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		sw.Header().Set("X-Request-Id", reqID)
		ctx := rq.Context()

		defer func() {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			r.metrics.Record(op.ID, status)
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			r.log.LogAttrs(ctx, level, "request",
				slog.String("request_id", reqID),
				slog.String("service", op.Service),
				slog.String("op", op.ID),
				slog.String("method", op.Method),
				slog.String("path", rq.URL.Path),
				slog.Int("status", status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
		}()

		defer func() {
			if rec := recover(); rec != nil {
				r.log.LogAttrs(ctx, slog.LevelError, "panic serving request",
					slog.String("request_id", reqID),
					slog.String("op", op.ID),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				apierr.Write(sw, apierr.Internal("internal error"))
			}
		}()

		if op.Auth != api.AuthPublic {
			if r.auth == nil {
				apierr.Write(sw, apierr.Internal("authenticator not configured").WithDetail("op", op.ID))
				return
			}
			var (
				principal Principal
				err       error
			)
			if op.Auth == api.AuthPlayer {
				principal, err = r.auth.Player(rq)
			} else {
				principal, err = r.auth.Service(rq)
			}
			if err != nil {
				apierr.Write(sw, err)
				return
			}
			ctx = WithPrincipal(ctx, principal)
		}

		out, err := h(rq.WithContext(ctx))
		if err != nil {
			apierr.Write(sw, err)
			return
		}
		if out == nil {
			sw.WriteHeader(http.StatusNoContent)
			return
		}
		WriteJSON(sw, http.StatusOK, out)
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
