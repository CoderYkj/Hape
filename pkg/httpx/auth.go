package httpx

import (
	"context"
	"net/http"
	"strings"

	"gamebackend/pkg/apierr"
)

// Principal is the authenticated caller. For a player, both fields are set: the
// backend player id (what the API exposes) and the external identity that produced
// it (Nakama's user id, useful for logs and support).
type Principal struct {
	PlayerID   string
	ExternalID string
	Service    string
}

// IsPlayer reports whether the caller is an end player.
func (p Principal) IsPlayer() bool { return p.PlayerID != "" }

type principalKey struct{}

// WithPrincipal stores the authenticated caller on the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom reads the caller injected by the router.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator turns a request into a Principal.
//
// Production: Nakama-issued JWT for players, mTLS/HMAC for services.
// Dev: bearer strings, see DevAuth.
type Authenticator interface {
	Player(r *http.Request) (Principal, error)
	Service(r *http.Request) (Principal, error)
}

// DevAuth is the local-development authenticator. It is NEVER valid in prod:
// the platform refuses to start with it when BACKEND_ENV=prod.
//
//	Authorization: Bearer dev-player-<player_id>
//	Authorization: Bearer dev-service
type DevAuth struct {
	ServiceToken string
	// Credentials accepts several dev tokens at once, so the rotation path is
	// exercisable locally and in tests rather than only in production.
	Credentials ServiceCredentials
}

// NewDevAuth returns the development authenticator for one service token.
func NewDevAuth(serviceToken string) *DevAuth {
	if serviceToken == "" {
		serviceToken = DevServiceToken
	}
	return &DevAuth{
		ServiceToken: serviceToken,
		Credentials:  ServiceCredentials{{Name: LegacyServiceCredentialName, Token: serviceToken}},
	}
}

// NewDevAuthWithCredentials returns the development authenticator for a set of tokens.
func NewDevAuthWithCredentials(creds ServiceCredentials) *DevAuth {
	if len(creds) == 0 {
		return NewDevAuth("")
	}
	return &DevAuth{Credentials: creds}
}

const devPlayerPrefix = "dev-player-"

// Player implements Authenticator.
func (d *DevAuth) Player(r *http.Request) (Principal, error) {
	token := bearer(r)
	if !strings.HasPrefix(token, devPlayerPrefix) {
		return Principal{}, apierr.Unauthenticated("missing or invalid player token")
	}
	id := strings.TrimPrefix(token, devPlayerPrefix)
	if id == "" {
		return Principal{}, apierr.Unauthenticated("missing or invalid player token")
	}
	return Principal{PlayerID: id}, nil
}

// Service implements Authenticator.
func (d *DevAuth) Service(r *http.Request) (Principal, error) {
	creds := d.Credentials
	if len(creds) == 0 && d.ServiceToken != "" {
		creds = ServiceCredentials{{Name: LegacyServiceCredentialName, Token: d.ServiceToken}}
	}
	name, ok := creds.Lookup(bearer(r))
	if !ok {
		return Principal{}, apierr.Unauthenticated("missing or invalid service token")
	}
	return Principal{Service: name}, nil
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return h[7:]
	}
	return ""
}
