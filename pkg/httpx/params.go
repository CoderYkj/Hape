package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gamebackend/pkg/apierr"
)

// maxBodyBytes caps request bodies. A game backend accepts small JSON only.
const maxBodyBytes = 1 << 20

// Decode reads a JSON body into v, rejecting unknown fields so a typo in a
// client is a 400 instead of a silently ignored setting.
func Decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apierr.Invalid("malformed JSON body").WithCause(err)
	}
	return nil
}

// Param returns a path wildcard value.
func Param(r *http.Request, name string) string { return r.PathValue(name) }

// RequireSelf enforces that a player may only act on their own resource.
//
// Authorization lives here, in one place, instead of being re-implemented by
// every handler: "is this the caller's own player_id?" is the answer 90% of the
// time and forgetting it once is a data breach.
func RequireSelf(r *http.Request, playerID string) error {
	principal, ok := PrincipalFrom(r.Context())
	if !ok || !principal.IsPlayer() {
		return apierr.Unauthenticated("player session required")
	}
	if principal.PlayerID != playerID {
		return apierr.PermissionDenied("player may only access their own resources")
	}
	return nil
}

// RequirePlayer returns the authenticated player id.
func RequirePlayer(r *http.Request) (string, error) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok || !principal.IsPlayer() {
		return "", apierr.Unauthenticated("player session required")
	}
	return principal.PlayerID, nil
}

// RequireService reports whether the caller is an internal service.
func RequireService(r *http.Request) (string, error) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok || principal.IsPlayer() || principal.Service == "" {
		return "", apierr.PermissionDenied("service token required")
	}
	return principal.Service, nil
}

// IdempotencyKey returns the caller-supplied key. Retryable write operations
// require one, so a duplicated battle report cannot pay a player twice.
func IdempotencyKey(r *http.Request) (string, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", apierr.Invalid("Idempotency-Key header is required").WithDetail("header", "Idempotency-Key")
	}
	if len(key) > 128 {
		return "", apierr.Invalid("Idempotency-Key must be at most 128 characters").WithDetail("header", "Idempotency-Key")
	}
	return key, nil
}

// QueryInt reads an integer query parameter, clamped to [1, max]. Absent means def.
//
// Clamping rather than rejecting an oversized limit keeps a misconfigured dashboard from
// turning into a 400 on a read path, while still bounding the response. A non-numeric
// value is a client bug and is refused.
func QueryInt(r *http.Request, name string, def, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, apierr.Invalid("query parameter "+name+" must be an integer").
			WithDetail("parameter", name)
	}
	if n < 1 {
		return 0, apierr.Invalid("query parameter "+name+" must be positive").
			WithDetail("parameter", name)
	}
	if max > 0 && n > max {
		n = max
	}
	return n, nil
}
