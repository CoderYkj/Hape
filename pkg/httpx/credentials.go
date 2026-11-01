package httpx

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ServiceCredential is one server-to-server identity: a name and a secret.
//
// Several credentials are accepted at once so a secret can be rotated without
// downtime: deploy with the new one added, move the callers over one by one, deploy
// again with the old one removed. The name is what makes that observable - logs say
// which caller a request came from, so "is anything still using the old credential"
// is answerable instead of a guess.
//
// Rotation is not least privilege. Every credential can still reach every
// service-auth op; scoping credentials per caller is a separate change (spec 0014).
type ServiceCredential struct {
	Name  string
	Token string
}

// ServiceCredentials is the set of credentials this process accepts.
type ServiceCredentials []ServiceCredential

const (
	// DevServiceToken is what every local tool presents (fakeds, the minimal client,
	// curl examples). Development only: prod refuses dev auth entirely.
	DevServiceToken = "dev-service"
	// LegacyServiceCredentialName is the name given to a credential that came from the
	// single-token variable. It authenticates; it just cannot be rotated per caller.
	LegacyServiceCredentialName = "service"
)

var credentialNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ParseServiceCredentials reads the `name=token,name=token` form.
//
// The syntax is deliberately boring: names are restricted because they end up in log
// lines and dashboards, and tokens may not contain a comma (base64url never does).
func ParseServiceCredentials(raw string) (ServiceCredentials, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out ServiceCredentials
	seenName := map[string]bool{}
	seenToken := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, token, ok := strings.Cut(entry, "=")
		name, token = strings.TrimSpace(name), strings.TrimSpace(token)
		if !ok || name == "" || token == "" {
			return nil, fmt.Errorf("service credential %q is not in name=token form", entry)
		}
		if !credentialNamePattern.MatchString(name) {
			return nil, fmt.Errorf("service credential name %q must match %s", name, credentialNamePattern)
		}
		if seenName[name] {
			return nil, fmt.Errorf("duplicate service credential name %q", name)
		}
		// Two names on one secret would defeat the point: retiring one retires both,
		// and the audit trail could not tell them apart.
		if seenToken[token] {
			return nil, fmt.Errorf("service credential %q reuses another credential's token", name)
		}
		seenName[name] = true
		seenToken[token] = true
		out = append(out, ServiceCredential{Name: name, Token: token})
	}
	if len(out) == 0 {
		return nil, errors.New("service credentials are empty")
	}
	return out, nil
}

// Validate is the startup check. A credential set that duplicates itself, or violates
// the name rules, is a deployment mistake: it must fail now, not on the first request.
func (c ServiceCredentials) Validate() error {
	if len(c) == 0 {
		return errors.New("no service credentials configured")
	}
	seenName := map[string]bool{}
	seenToken := map[string]bool{}
	for _, cred := range c {
		if !credentialNamePattern.MatchString(cred.Name) {
			return fmt.Errorf("service credential name %q must match %s", cred.Name, credentialNamePattern)
		}
		if cred.Token == "" {
			return fmt.Errorf("service credential %q has an empty token", cred.Name)
		}
		if seenName[cred.Name] {
			return fmt.Errorf("duplicate service credential name %q", cred.Name)
		}
		if seenToken[cred.Token] {
			return fmt.Errorf("service credential %q reuses another credential's token", cred.Name)
		}
		seenName[cred.Name] = true
		seenToken[cred.Token] = true
	}
	return nil
}

// Lookup returns the name of the credential whose token matches.
//
// It compares against every credential and does not return early, so the time spent
// does not reveal which entry matched - the same reason the comparison itself is
// constant time.
func (c ServiceCredentials) Lookup(presented string) (string, bool) {
	if presented == "" {
		return "", false
	}
	matched := ""
	found := 0
	for _, cred := range c {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(cred.Token)) == 1 {
			matched = cred.Name
			found = 1
		}
	}
	return matched, found == 1
}

// Names lists the credential names, sorted. Startup logs and rotation checklists use
// it: "these identities can authenticate right now" is the fact an operator needs.
func (c ServiceCredentials) Names() []string {
	names := make([]string, 0, len(c))
	for _, cred := range c {
		names = append(names, cred.Name)
	}
	sort.Strings(names)
	return names
}
