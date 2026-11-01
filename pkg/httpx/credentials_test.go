package httpx_test

import (
	"strings"
	"testing"

	"gamebackend/pkg/httpx"
)

func TestParseServiceCredentials(t *testing.T) {
	creds, err := httpx.ParseServiceCredentials("nakama=abc123, worker = xyz789 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("got %d credentials, want 2", len(creds))
	}
	if creds[0].Name != "nakama" || creds[0].Token != "abc123" {
		t.Fatalf("first credential = %+v", creds[0])
	}
	// Whitespace around an entry is a formatting convenience, not part of the token.
	if creds[1].Name != "worker" || creds[1].Token != "xyz789" {
		t.Fatalf("second credential = %+v", creds[1])
	}
}

func TestParseServiceCredentialsRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"no equals sign":           "nakama",
		"empty name":               "=token",
		"empty token":              "nakama=",
		"uppercase name":           "Nakama=token",
		"name starts with a digit": "1nakama=token",
		"duplicate name":           "nakama=a,nakama=b",
		"duplicate token":          "nakama=a,worker=a",
	}
	for name, raw := range cases {
		if _, err := httpx.ParseServiceCredentials(raw); err == nil {
			t.Fatalf("%s should have been rejected: %q", name, raw)
		}
	}
}

func TestParseServiceCredentialsTreatsEmptyAsUnset(t *testing.T) {
	creds, err := httpx.ParseServiceCredentials("   ")
	if err != nil {
		t.Fatalf("blank input should be unset, not an error: %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("blank input produced %d credentials", len(creds))
	}
}

// TestLookupMatchesEveryEntry is the rotation contract in the small: several tokens are
// valid at once, and each one resolves to its own identity.
func TestLookupMatchesEveryEntry(t *testing.T) {
	creds := httpx.ServiceCredentials{
		{Name: "old", Token: "old-secret"},
		{Name: "new", Token: "new-secret"},
	}
	for token, want := range map[string]string{"old-secret": "old", "new-secret": "new"} {
		cred, ok := creds.Lookup(token)
		if !ok {
			t.Fatalf("%q was not accepted", token)
		}
		if cred.Name != want {
			t.Fatalf("%q resolved to %q, want %q", token, cred.Name, want)
		}
	}
	for _, bad := range []string{"", "old-secret ", "unknown"} {
		if _, ok := creds.Lookup(bad); ok {
			t.Fatalf("%q must not authenticate", bad)
		}
	}
}

func TestValidateRejectsDuplicatesBuiltInCode(t *testing.T) {
	// Parse rejects these, but a caller can build the slice directly; Validate is the
	// last line before the authenticator starts trusting it.
	cases := map[string]httpx.ServiceCredentials{
		"empty": {},
		"duplicate name": {
			{Name: "a", Token: "1"},
			{Name: "a", Token: "2"},
		},
		"duplicate token": {
			{Name: "a", Token: "1"},
			{Name: "b", Token: "1"},
		},
		"empty token": {{Name: "a"}},
		"bad name":    {{Name: "A", Token: "1"}},
	}
	for name, creds := range cases {
		if err := creds.Validate(); err == nil {
			t.Fatalf("%s should have been rejected", name)
		}
	}
}

func TestNamesAreSorted(t *testing.T) {
	creds := httpx.ServiceCredentials{
		{Name: "worker", Token: "1"},
		{Name: "nakama", Token: "2"},
	}
	names := creds.Names()
	if strings.Join(names, ",") != "nakama,worker" {
		t.Fatalf("names = %v, want sorted [nakama worker]", names)
	}
}

// TestScopesParseAndDefaultToDeny pins the least-privilege grammar: `name=token:a+b`,
// `*` for everything, and *no* scope meaning none (a credential that authenticates but
// can call nothing is a configuration mistake, not a wildcard).
func TestScopesParseAndDefaultToDeny(t *testing.T) {
	creds, err := httpx.ParseServiceCredentials("nakama=abc:account,worker=xyz:matchmaking+session,ops=op:*,bare=def")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byName := map[string]httpx.ServiceCredential{}
	for _, cred := range creds {
		byName[cred.Name] = cred
	}
	if !byName["nakama"].Allows("account") || byName["nakama"].Allows("economy") {
		t.Fatalf("nakama scopes = %v", byName["nakama"].Services)
	}
	if !byName["worker"].Allows("session") || !byName["worker"].Allows("matchmaking") {
		t.Fatalf("worker scopes = %v", byName["worker"].Services)
	}
	if !byName["ops"].Allows("economy") || !byName["ops"].Allows("anything") {
		t.Fatalf("wildcard should allow everything: %v", byName["ops"].Services)
	}
	if len(byName["bare"].Services) != 0 || byName["bare"].Allows("account") {
		t.Fatalf("an omitted scope must deny, got %v", byName["bare"].Services)
	}
}

func TestScopesRejectAmbiguousInput(t *testing.T) {
	cases := map[string]string{
		"wildcard plus a name": "a=t:*+account",
		"empty scope entry":    "a=t:account+",
		"invalid service name": "a=t:Account",
		"duplicate service":    "a=t:account+account",
		"colon in the token":   "a=t:account=x",
	}
	for name, raw := range cases {
		if _, err := httpx.ParseServiceCredentials(raw); err == nil {
			t.Fatalf("%s should have been rejected: %q", name, raw)
		}
	}
}

// TestUnknownServicesAreReported is the startup guard against a typo in a scope: a name
// that is not a service this process serves can never match, so it would deny silently.
func TestUnknownServicesAreReported(t *testing.T) {
	creds := httpx.ServiceCredentials{
		{Name: "a", Token: "1", Services: []string{"account", "econmy"}},
		{Name: "b", Token: "2", Services: []string{"*"}},
	}
	unknown := creds.UnknownServices(map[string]bool{"account": true, "session": true})
	if len(unknown) != 1 || unknown[0] != "a:econmy" {
		t.Fatalf("unknown = %v, want [a:econmy]", unknown)
	}
}
