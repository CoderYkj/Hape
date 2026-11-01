package httpx

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ServiceCredential is one server-to-server identity: a name, a secret, and the services
// it may call.
//
// Several credentials are accepted at once so a secret can be rotated without downtime:
// deploy with the new one added, move the callers over one by one, deploy again with the
// old one removed. The name is what makes that observable - logs say which caller a
// request came from, so "is anything still using the old credential" is answerable
// instead of a guess.
//
// Services is least privilege. A credential that is not allowed to call a service gets a
// 403 from the router; omitting the scope entirely means "no services", not "all of
// them": a credential that can authenticate but call nothing is a configuration mistake
// worth failing on immediately rather than a silent hole.
type ServiceCredential struct {
	Name  string
	Token string
	// Services lists the service names this credential may call. "*" means all of them,
	// and must be alone.
	Services []string
}

// Allows reports whether this credential may call the given service.
func (c ServiceCredential) Allows(service string) bool {
	for _, allowed := range c.Services {
		if allowed == "*" || allowed == service {
			return true
		}
	}
	return false
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
	// AllServices is the wildcard scope.
	AllServices = "*"
)

var (
	credentialNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	serviceNamePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// ParseServiceCredentials reads `name=token[:service+service]`, comma separated.
//
// The syntax is deliberately boring: names are restricted because they end up in log
// lines and dashboards; tokens may not contain a comma or a colon, which is what keeps
// the scope suffix unambiguous (no credential format in use here needs either).
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
		name, rest, ok := strings.Cut(entry, "=")
		name, rest = strings.TrimSpace(name), strings.TrimSpace(rest)
		if !ok || name == "" || rest == "" {
			return nil, fmt.Errorf("service credential %q is not in name=token form", entry)
		}
		if !credentialNamePattern.MatchString(name) {
			return nil, fmt.Errorf("service credential name %q must match %s", name, credentialNamePattern)
		}

		token, scope, hasScope := strings.Cut(rest, ":")
		token = strings.TrimSpace(token)
		if token == "" {
			return nil, fmt.Errorf("service credential %q has an empty token", name)
		}
		if strings.ContainsAny(token, ",:") {
			return nil, fmt.Errorf("service credential %q: the token must not contain ',' or ':'", name)
		}

		services, err := parseScope(name, scope, hasScope)
		if err != nil {
			return nil, err
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
		out = append(out, ServiceCredential{Name: name, Token: token, Services: services})
	}
	if len(out) == 0 {
		return nil, errors.New("service credentials are empty")
	}
	return out, nil
}

func parseScope(name, scope string, hasScope bool) ([]string, error) {
	if !hasScope || strings.TrimSpace(scope) == "" {
		// No scope: authenticate but call nothing. Fail closed and loudly, see the
		// ServiceCredential doc comment.
		return nil, nil
	}
	parts := strings.Split(scope, "+")
	services := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("service credential %q has an empty scope entry (check the '+' separators)", name)
		}
		if part != AllServices && !serviceNamePattern.MatchString(part) {
			return nil, fmt.Errorf("service credential %q has an invalid service %q", name, part)
		}
		if seen[part] {
			return nil, fmt.Errorf("service credential %q lists %q twice", name, part)
		}
		seen[part] = true
		services = append(services, part)
	}
	if seen[AllServices] && len(services) > 1 {
		return nil, fmt.Errorf("service credential %q combines %q with named services; use one or the other", name, AllServices)
	}
	return services, nil
}

// Validate is the startup check. A credential set that duplicates itself, violates the
// name rules, or declares a scope nothing can satisfy is a deployment mistake: it must
// fail now, not on the first request.
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

		wildcard := 0
		for _, service := range cred.Services {
			if service == AllServices {
				wildcard++
				continue
			}
			if !serviceNamePattern.MatchString(service) {
				return fmt.Errorf("service credential %q has an invalid service %q", cred.Name, service)
			}
		}
		if wildcard > 1 || (wildcard == 1 && len(cred.Services) > 1) {
			return fmt.Errorf("service credential %q: %q must stand alone", cred.Name, AllServices)
		}
	}
	return nil
}

// Lookup returns the credential whose token matches.
//
// It compares against every credential and does not return early, so the time spent does
// not reveal which entry matched - the same reason the comparison itself is constant time.
func (c ServiceCredentials) Lookup(presented string) (ServiceCredential, bool) {
	var (
		matched ServiceCredential
		found   int
	)
	if presented == "" {
		return ServiceCredential{}, false
	}
	for _, cred := range c {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(cred.Token)) == 1 {
			matched = cred
			found = 1
		}
	}
	if found != 1 {
		return ServiceCredential{}, false
	}
	return matched, true
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

// Describe renders the credential set for a startup log line: names with their scopes,
// never tokens.
func (c ServiceCredentials) Describe() []string {
	out := make([]string, 0, len(c))
	for _, cred := range c {
		scope := "none"
		if len(cred.Services) > 0 {
			scope = strings.Join(cred.Services, "+")
		}
		out = append(out, cred.Name+"("+scope+")")
	}
	sort.Strings(out)
	return out
}

// UnknownServices returns the scope entries that are not services this process serves.
//
// A scope naming a service that does not exist is always a typo, and a typo here means
// the credential silently cannot call the thing it was meant to: the router denies on
// the real name, and nobody notices until the feature is used.
func (c ServiceCredentials) UnknownServices(known map[string]bool) []string {
	var unknown []string
	for _, cred := range c {
		for _, service := range cred.Services {
			if service == AllServices || known[service] {
				continue
			}
			unknown = append(unknown, cred.Name+":"+service)
		}
	}
	sort.Strings(unknown)
	return unknown
}
