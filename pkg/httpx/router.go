package httpx

import (
	"context"
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
	// enforceScopes turns the least-privilege check into a refusal. It defaults to true;
	// observe mode (log-only) exists so a scope policy can be validated against real
	// traffic before it starts refusing anything.
	enforceScopes bool
}

// NewRouter builds a router. auth may be nil only when no authenticated op is
// registered (tests of a single public endpoint).
func NewRouter(service string, log *slog.Logger, auth Authenticator) *Router {
	if log == nil {
		log = slog.Default()
	}
	metrics := NewMetrics(service)
	// Register the scope counters at zero: the observe-mode rollout is judged by
	// "would-deny is zero", and a counter that only appears after the first event makes
	// that query indistinguishable from "the metric is broken".
	metrics.AddCounter("service_scope_denied", 0)
	metrics.AddCounter("service_scope_would_deny", 0)

	return &Router{
		service:       service,
		auth:          auth,
		log:           log,
		metrics:       metrics,
		mux:           http.NewServeMux(),
		enforceScopes: true,
	}
}

// SetScopeEnforcement switches the least-privilege check between refusing (default) and
// only logging what it would refuse.
//
// Observe mode is the rollout path: run it against real traffic, watch for
// "service credential out of scope (not enforced)" lines, fix the scopes, then enforce.
// Enforcing a policy nobody validated is how a deployment loses an op it needed.
func (r *Router) SetScopeEnforcement(enforce bool) { r.enforceScopes = enforce }

// Metrics returns the registry this router publishes. Domain services use it to add
// counters and gauges to the same /metrics endpoint.
func (r *Router) Metrics() *Metrics { return r.metrics }

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
			r.metrics.Observe(op.ID, time.Since(start).Seconds())
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			attrs := []slog.Attr{
				slog.String("request_id", reqID),
				// "op_service" rather than "service": the logger already carries the
				// process name under "service", and a duplicate JSON key means whichever
				// one the parser keeps wins - the losing one silently disappears.
				slog.String("op_service", op.Service),
				slog.String("op", op.ID),
			}
			// Which credential authenticated the call: without it, "was the old token
			// retired?" is unanswerable from the logs.
			if caller, ok := PrincipalFrom(ctx); ok && caller.Service != "" {
				attrs = append(attrs, slog.String("caller", caller.Service))
			}
			attrs = append(attrs,
				slog.String("method", op.Method),
				slog.String("path", rq.URL.Path),
				slog.Int("status", status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
			r.log.LogAttrs(ctx, level, "request", attrs...)
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

			// Least privilege for service callers. Players are already scoped by the op
			// itself (AuthPlayer), so there is nothing to compare for them.
			if op.Auth == api.AuthService && !r.scopeAllows(ctx, op, principal) {
				apierr.Write(sw, apierr.PermissionDenied(
					"service credential is not allowed to call this service").
					WithDetail("caller", principal.Service).
					WithDetail("service", op.Service))
				return
			}
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

// scopeAllows is the least-privilege decision for one call: does this credential's scope
// cover the service that owns the operation?
//
// Empty scopes deny: a credential that can authenticate but call nothing is a
// configuration mistake, and failing closed is the only safe default for a permission
// check.
func (r *Router) scopeAllows(ctx context.Context, op api.Op, p Principal) bool {
	for _, allowed := range p.Scopes {
		if allowed == AllServices || allowed == op.Service {
			return true
		}
	}
	if !r.enforceScopes {
		// Observe mode: say exactly what enforcement would have refused, with the
		// identity and the service, because that line is the input to fixing scopes.
		// The counter is what the rollout is judged on: "would-deny has been zero for a
		// day" is a query, not a log grep.
		r.metrics.AddCounter("service_scope_would_deny", 1)
		r.log.WarnContext(ctx, "service credential out of scope (not enforced)",
			"caller", p.Service, "op", op.ID, "op_service", op.Service, "scopes", p.Scopes)
		return true
	}
	r.metrics.AddCounter("service_scope_denied", 1)
	return false
}
