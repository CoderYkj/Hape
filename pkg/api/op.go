// Package api is the contract layer.
//
// Every operation a backend service exposes is declared exactly once, as an
// Op value, next to the handler that serves it. The OpenAPI document is
// generated from those declarations (tools/apigen), so the published contract
// can never drift away from the code. See .harness/rules/30-api-contract.md.
package api

import (
	"net/http"
	"sort"
	"strings"
)

// AuthKind says who may call the operation.
type AuthKind string

const (
	// AuthPublic is reachable without credentials (login, health).
	AuthPublic AuthKind = "public"
	// AuthPlayer requires a player session token and injects a Principal.
	AuthPlayer AuthKind = "player"
	// AuthService requires a service token: server-to-server only.
	AuthService AuthKind = "service"
)

// SecurityScheme is the OpenAPI security scheme name for the kind.
func (a AuthKind) SecurityScheme() string {
	switch a {
	case AuthPlayer:
		return "playerToken"
	case AuthService:
		return "serviceToken"
	}
	return ""
}

// Op is one HTTP operation. Path uses the http.ServeMux wildcard syntax
// ("/v1/account/players/{player_id}") which is also valid OpenAPI.
type Op struct {
	ID         string // stable id, e.g. "account.get_player"; used as operationId
	Service    string // owning service: account, team, matchmaking, ...
	Method     string // http.MethodGet, ...
	Path       string // ServeMux pattern with {wildcards}
	Summary    string
	Request    *Shape // nil means "no request body"
	Response   Shape
	Auth       AuthKind
	Idempotent bool   // safe to retry with the same Idempotency-Key
	Note       string // implementation note; not published in OpenAPI
}

// PathParams returns the wildcard names of the pattern, in order.
func (o Op) PathParams() []string {
	path := o.Path
	var out []string
	for {
		i := strings.IndexByte(path, '{')
		if i < 0 {
			return out
		}
		rest := path[i+1:]
		j := strings.IndexByte(rest, '}')
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		path = rest[j+1:]
	}
}

// Services returns the distinct service names, sorted.
func Services(ops []Op) []string {
	seen := map[string]bool{}
	var out []string
	for _, op := range ops {
		if !seen[op.Service] {
			seen[op.Service] = true
			out = append(out, op.Service)
		}
	}
	sort.Strings(out)
	return out
}

// ForService filters ops by owning service.
func ForService(ops []Op, service string) []Op {
	var out []Op
	for _, op := range ops {
		if op.Service == service {
			out = append(out, op)
		}
	}
	return out
}

var validMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPatch: true,
	http.MethodPut: true, http.MethodDelete: true,
}

// Validate enforces the invariants of the contract. Callers run it in tests,
// so a malformed declaration fails the build instead of shipping.
func Validate(ops []Op) error {
	ids := map[string]bool{}
	routes := map[string]string{}
	for _, op := range ops {
		if op.ID == "" || !strings.Contains(op.ID, ".") {
			return errf("op %q: ID must look like \"<service>.<verb>\"", op.ID)
		}
		if ids[op.ID] {
			return errf("duplicate operation id %q", op.ID)
		}
		ids[op.ID] = true
		if op.Service == "" {
			return errf("op %q: missing Service", op.ID)
		}
		if !validMethods[op.Method] {
			return errf("op %q: unsupported method %q", op.ID, op.Method)
		}
		if !strings.HasPrefix(op.Path, "/") {
			return errf("op %q: path must start with /", op.ID)
		}
		route := op.Method + " " + op.Path
		if other, ok := routes[route]; ok {
			return errf("ops %q and %q share route %s", other, op.ID, route)
		}
		routes[route] = op.ID
		if op.Summary == "" {
			return errf("op %q: missing Summary", op.ID)
		}
		if op.Response.Type == "" {
			return errf("op %q: missing Response shape", op.ID)
		}
		switch op.Auth {
		case AuthPublic, AuthPlayer, AuthService:
		default:
			return errf("op %q: bad Auth %q", op.ID, op.Auth)
		}
	}
	return nil
}
