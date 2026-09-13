package core

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// ClusterEndpoint is a cluster the job gateway can route to.
//
// One hostname per cluster: the stock `ray job submit` client hits fixed
// root paths (/api/jobs/, /api/packages/…) on its --address, so the
// cluster identity must live in the host, not the path (ADR-0002).
type ClusterEndpoint struct {
	Id ClusterId `json:"id"`
	// Hostname (without port) at which the gateway exposes this cluster.
	Hostname string `json:"hostname"`
	// ApiBaseUrl is the base URL of the cluster's native Ray
	// dashboard/job API, reachable from the control plane only.
	ApiBaseUrl string `json:"api_base_url"`
	// AuthToken is the static Ray auth token (Ray >= 2.52). The gateway
	// injects it southbound; users never see it (ADR-0003). Excluded
	// from serialization so it can't leak through API responses.
	AuthToken *string `json:"auth_token,omitempty"`
	// AuthTokenEnv is the name of the environment variable to read the
	// auth token from at load time — secret indirection so the registry
	// file holds no plaintext credential (compliance issue #57).
	// Mutually exclusive with AuthToken; unlike the token, the name is
	// not a secret and may serialize.
	AuthTokenEnv *string `json:"auth_token_env,omitempty"`
	// Project is the project that owns the workload behind this entry, so
	// the gateway can scope authorization per project (#5/#2). "" for
	// static entries that predate project scoping.
	Project string `json:"project,omitempty"`
	// Target is what the hostname fronts: RegistryTargetJobs (the Ray
	// Jobs API — the default when absent) or RegistryTargetServe (a Serve
	// application's HTTP endpoint).
	Target string `json:"target,omitempty"`
	// Source is how the entry got into the registry: RegistrySourceStatic
	// (the --registry file) or RegistrySourceDynamic (registered at run
	// time by the lifecycle controller). Stamped by the registry on every
	// lookup/snapshot; a file entry never needs to set it.
	Source string `json:"source,omitempty"`
}

// Registry entry Target / Source vocabularies (the contract's
// RegistryEntryView enums).
const (
	RegistryTargetJobs  = "jobs"
	RegistryTargetServe = "serve"

	RegistrySourceStatic  = "static"
	RegistrySourceDynamic = "dynamic"
)

// clusterEndpointAlias breaks the recursion MarshalJSON would otherwise
// cause by re-entering ClusterEndpoint's own MarshalJSON.
type clusterEndpointAlias ClusterEndpoint

// MarshalJSON excludes AuthToken from the wire representation entirely —
// it is read-only on unmarshal (mirrors Rust's
// #[serde(default, skip_serializing)]).
func (c ClusterEndpoint) MarshalJSON() ([]byte, error) {
	a := clusterEndpointAlias(c)
	a.AuthToken = nil
	return json.Marshal(a)
}

// String is a manual, redacting Stringer: the auth token must never reach
// logs — the MarshalJSON above protects API responses, this protects
// log/panic output (security issue #4).
func (c ClusterEndpoint) String() string {
	token := "None"
	if c.AuthToken != nil {
		token = "[REDACTED]"
	}
	env := "None"
	if c.AuthTokenEnv != nil {
		env = fmt.Sprintf("Some(%q)", *c.AuthTokenEnv)
	}
	return fmt.Sprintf(
		"ClusterEndpoint{Id: %s, Hostname: %s, ApiBaseUrl: %s, AuthToken: %s, AuthTokenEnv: %s}",
		c.Id, c.Hostname, c.ApiBaseUrl, token, env,
	)
}

// ClusterRegistry is the gateway's routing table: the static entries
// loaded from the --registry file at boot (Clusters, immutable after
// load) plus the dynamic entries the lifecycle controller registers and
// deregisters as it provisions ephemeral clusters (#5). Lookups consult
// static first, then dynamic, under a read lock; a dynamic entry can never
// shadow a static hostname (Upsert refuses it), so the file stays the
// operator's override.
//
// Contains a mutex: pass it around as *ClusterRegistry, never by value
// (`go vet` copylocks).
type ClusterRegistry struct {
	Clusters []ClusterEndpoint `json:"clusters"`

	mu      sync.RWMutex
	dynamic map[ClusterId]ClusterEndpoint
}

// clusterRegistryWire is the JSON shape: only the static entries are the
// file format — dynamic entries are runtime state, not configuration.
type clusterRegistryWire struct {
	Clusters []ClusterEndpoint `json:"clusters"`
}

// MarshalJSON substitutes an empty slice for a nil Clusters, mirroring
// Rust's Vec::default(), which serde always writes as `[]`, never `null`.
// Pointer receiver: the struct carries a mutex.
func (r *ClusterRegistry) MarshalJSON() ([]byte, error) {
	w := clusterRegistryWire{Clusters: r.Clusters}
	if w.Clusters == nil {
		w.Clusters = []ClusterEndpoint{}
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads the file format (static entries only).
func (r *ClusterRegistry) UnmarshalJSON(data []byte) error {
	var w clusterRegistryWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	r.Clusters = w.Clusters
	return nil
}

// String redacts every entry's auth token (see ClusterEndpoint.String).
// Static and dynamic entries both appear, in Snapshot order.
func (r *ClusterRegistry) String() string {
	all := r.Snapshot()
	parts := make([]string, len(all))
	for i := range all {
		parts[i] = all[i].String()
	}
	return fmt.Sprintf("ClusterRegistry{Clusters: [%s]}", strings.Join(parts, ", "))
}

// stamp returns c with Source set and Target defaulted — the one place
// egress normalization happens, so a caller never sees an entry whose
// provenance or target is ambiguous.
func stamp(c ClusterEndpoint, source string) ClusterEndpoint {
	c.Source = source
	if c.Target == "" {
		c.Target = RegistryTargetJobs
	}
	return c
}

// Upsert registers (or replaces, by Id) a dynamic entry. It refuses an
// entry that would shadow a static hostname or reuse a static id (the
// file is the operator's override, and first-match-wins misrouting is the
// exact failure Validate guards against), and one whose hostname another
// dynamic entry already routes. Hostnames compare case-insensitively.
//
// ApiBaseUrl is validated too (F6): scheme restricted to http/https, no
// userinfo/fragment, and literal IPs in link-local/CGNAT/loopback/
// unspecified ranges refused — a runtime-registered endpoint that points
// at the gateway host itself or a metadata endpoint is never legitimate.
// Hosts that only some resolver would read as a literal (an empty host —
// dialed as localhost by Go's dialer — trailing-dot FQDN forms, and
// inet_aton-shaped numerics) are refused outright as well.
// Cluster-internal DNS names (the controller's <head-svc>.<ns>.svc form)
// and RFC 1918/ULA literals pass: a head observed at a cluster-private pod
// IP is a real endpoint, and Validate's operator opt-in does not apply to
// runtime state.
func (r *ClusterRegistry) Upsert(c ClusterEndpoint) error {
	if c.Hostname == "" || hasInvalidHostnameChar(c.Hostname) {
		return RegistryError{Kind: RegistryErrInvalidHostname, Id: string(c.Id), Hostname: c.Hostname}
	}
	invalid := func(reason string) error {
		return RegistryError{Kind: RegistryErrInvalidUrl, Id: string(c.Id), Url: c.ApiBaseUrl, Reason: reason}
	}
	reason, authority := urlShapeError(c.ApiBaseUrl)
	if reason != "" {
		return invalid(reason)
	}
	if ip := net.ParseIP(authorityHost(authority)); ip != nil {
		if isDeniedSouthboundIP(ip) {
			return invalid("literal IP in a link-local/CGNAT range (169.254.0.0/16, " +
				"100.64.0.0/10, fe80::/10) is not a cluster endpoint")
		}
		if ip.IsLoopback() || ip.IsUnspecified() {
			return invalid("literal loopback/unspecified IP is not a dynamic cluster endpoint")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.Clusters {
		if r.Clusters[i].Id == c.Id {
			return RegistryError{Kind: RegistryErrDuplicateId, Id: string(c.Id)}
		}
		if strings.EqualFold(r.Clusters[i].Hostname, c.Hostname) {
			return RegistryError{Kind: RegistryErrDuplicateHostname, Hostname: c.Hostname}
		}
	}
	for id, d := range r.dynamic {
		if id != c.Id && strings.EqualFold(d.Hostname, c.Hostname) {
			return RegistryError{Kind: RegistryErrDuplicateHostname, Hostname: c.Hostname}
		}
	}
	if r.dynamic == nil {
		r.dynamic = map[ClusterId]ClusterEndpoint{}
	}
	r.dynamic[c.Id] = c
	return nil
}

// Remove deregisters a dynamic entry. Static entries cannot be removed at
// run time. Returns whether an entry was removed.
func (r *ClusterRegistry) Remove(id ClusterId) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.dynamic[id]
	if ok {
		delete(r.dynamic, id)
	}
	return ok
}

// Register is Upsert under the controller-facing verb (see
// controller.Registrar).
func (r *ClusterRegistry) Register(c ClusterEndpoint) error { return r.Upsert(c) }

// Deregister is Remove under the controller-facing verb (see
// controller.Registrar).
func (r *ClusterRegistry) Deregister(id ClusterId) { r.Remove(id) }

// Snapshot returns a copy of every entry — static entries in file order,
// then dynamic entries ordered by id — each stamped with its Source and an
// effective Target. Copies, not aliases: a caller cannot reach the stored
// AuthToken through the result.
func (r *ClusterRegistry) Snapshot() []ClusterEndpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ClusterEndpoint, 0, len(r.Clusters)+len(r.dynamic))
	for i := range r.Clusters {
		out = append(out, stamp(r.Clusters[i], RegistrySourceStatic))
	}
	ids := make([]ClusterId, 0, len(r.dynamic))
	for id := range r.dynamic {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		out = append(out, stamp(r.dynamic[id], RegistrySourceDynamic))
	}
	return out
}

// RegistryErrorKind discriminates ClusterRegistry failures.
type RegistryErrorKind int

const (
	RegistryErrDuplicateHostname RegistryErrorKind = iota
	RegistryErrDuplicateId
	RegistryErrInvalidUrl
	RegistryErrCleartextToken
	RegistryErrInvalidHostname
	RegistryErrConflictingTokenSource
	RegistryErrMissingTokenEnv
)

// RegistryError reports why a registry entry failed validation or token
// resolution.
type RegistryError struct {
	Kind RegistryErrorKind
	// Id is the offending cluster id, set on every variant except
	// DuplicateHostname.
	Id string
	// Hostname is set for DuplicateHostname and InvalidHostname.
	Hostname string
	// Url and Reason are set for InvalidUrl.
	Url    string
	Reason string
	// Var is set for MissingTokenEnv.
	Var string
}

func (e RegistryError) Error() string {
	switch e.Kind {
	case RegistryErrDuplicateHostname:
		return fmt.Sprintf("duplicate hostname %q: first match wins would silently misroute credentials", e.Hostname)
	case RegistryErrDuplicateId:
		return fmt.Sprintf("duplicate cluster id %q", e.Id)
	case RegistryErrInvalidUrl:
		return fmt.Sprintf("cluster %s: invalid api_base_url %q: %s", e.Id, e.Url, e.Reason)
	case RegistryErrCleartextToken:
		return fmt.Sprintf(
			"cluster %s: auth_token over cleartext http:// — refusing to ship a static "+
				"cluster credential unencrypted (use https, or pass an explicit insecure-transport "+
				"override for local dev)", e.Id)
	case RegistryErrInvalidHostname:
		return fmt.Sprintf("cluster %s: invalid hostname %q", e.Id, e.Hostname)
	case RegistryErrConflictingTokenSource:
		return fmt.Sprintf(
			"cluster %s: both auth_token and auth_token_env are set — exactly one token "+
				"source is allowed (issue #57)", e.Id)
	case RegistryErrMissingTokenEnv:
		return fmt.Sprintf(
			"cluster %s: auth_token_env %q is unset or empty — refusing to start "+
				"with a missing cluster credential", e.Id, e.Var)
	}
	return "registry error"
}

// TokenSourceNoteKind discriminates TokenSourceNote variants.
type TokenSourceNoteKind int

const (
	TokenSourceNotePlaintext TokenSourceNoteKind = iota
	TokenSourceNoteEnv
)

// TokenSourceNote reports where a registry entry's southbound token comes
// from — surfaced as startup log lines (#57). Carries names (cluster id,
// env var) only, never token values.
type TokenSourceNote struct {
	Kind TokenSourceNoteKind
	Id   string
	// Var is set for TokenSourceNoteEnv.
	Var string
}

// ByHostname looks up a cluster by request Host header value. Ports are
// ignored and matching is case-insensitive, per RFC 9110 host semantics.
// Returns a copy of the matched entry, not a pointer into the live slice —
// a caller must not be able to mutate the registry's stored entries (and
// in particular AuthToken) through a lookup result.
// Static entries win over dynamic ones.
func (r *ClusterRegistry) ByHostname(host string) (ClusterEndpoint, bool) {
	h := stripPort(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.Clusters {
		if strings.EqualFold(r.Clusters[i].Hostname, h) {
			return stamp(r.Clusters[i], RegistrySourceStatic), true
		}
	}
	for _, d := range r.dynamic {
		if strings.EqualFold(d.Hostname, h) {
			return stamp(d, RegistrySourceDynamic), true
		}
	}
	return ClusterEndpoint{}, false
}

// ByID looks up a cluster by id. Returns a copy of the matched entry, not
// a pointer into the live slice, for the same reason as ByHostname. Static
// entries win over dynamic ones.
func (r *ClusterRegistry) ByID(id ClusterId) (ClusterEndpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.Clusters {
		if r.Clusters[i].Id == id {
			return stamp(r.Clusters[i], RegistrySourceStatic), true
		}
	}
	if d, ok := r.dynamic[id]; ok {
		return stamp(d, RegistrySourceDynamic), true
	}
	return ClusterEndpoint{}, false
}

// ResolveAuthTokens resolves AuthTokenEnv indirections into AuthToken at
// load time (issue #57): each entry naming an env var has the token read
// from the process environment, so downstream gateway code sees one
// in-memory shape. Fails fast on a missing/empty variable, naming the
// cluster and the variable — never a value. An entry setting both token
// sources is rejected. AuthTokenEnv is kept set afterwards as provenance
// (it names the source; it is not a secret).
func (r *ClusterRegistry) ResolveAuthTokens() error {
	for i := range r.Clusters {
		c := &r.Clusters[i]
		if c.AuthToken != nil && c.AuthTokenEnv != nil {
			return RegistryError{Kind: RegistryErrConflictingTokenSource, Id: string(c.Id)}
		}
		if c.AuthTokenEnv != nil {
			v, ok := os.LookupEnv(*c.AuthTokenEnv)
			if !ok || v == "" {
				return RegistryError{Kind: RegistryErrMissingTokenEnv, Id: string(c.Id), Var: *c.AuthTokenEnv}
			}
			c.AuthToken = &v
		}
	}
	return nil
}

// TokenSourceNotes returns per-entry token-source notes for startup
// logging (#57): plaintext entries get a nudge toward AuthTokenEnv,
// env-sourced entries are acknowledged. Names only — never values.
func (r *ClusterRegistry) TokenSourceNotes() []TokenSourceNote {
	var notes []TokenSourceNote
	for _, c := range r.Clusters {
		switch {
		case c.AuthTokenEnv != nil:
			notes = append(notes, TokenSourceNote{Kind: TokenSourceNoteEnv, Id: string(c.Id), Var: *c.AuthTokenEnv})
		case c.AuthToken != nil:
			notes = append(notes, TokenSourceNote{Kind: TokenSourceNotePlaintext, Id: string(c.Id)})
		}
	}
	return notes
}

// ValidateOptions carries Validate's explicit danger overrides. Both
// default to the safe posture; the CLI surfaces them as DANGER flags.
type ValidateOptions struct {
	// AllowInsecureTransport permits a static auth token over cleartext
	// http:// southbound (local dev only).
	AllowInsecureTransport bool
	// AllowPrivateEndpoints permits api_base_urls whose host is a literal
	// IP in a loopback, unspecified, RFC 1918, or ULA (fc00::/7) range
	// (F6). Dev workflows legitimately point a static entry at a local
	// `ray start --head` on 127.0.0.1; in production a private-literal
	// southbound URL is an SSRF posture gap, so the allowance is an
	// explicit opt-in, never the default. Link-local/CGNAT ranges stay
	// denied under the opt-in — those never name a Ray head.
	AllowPrivateEndpoints bool
}

// Validate validates the registry as security-sensitive input (issues
// #2/#8, F6): duplicate hostnames/ids fail fast (first-match-wins
// misrouting), URLs are scheme-restricted with no userinfo/fragment,
// literal-IP hosts in link-local/CGNAT ranges are refused outright (SSRF:
// cloud metadata endpoints, overlay meshes), literal-IP hosts in
// loopback/unspecified/private ranges are refused unless explicitly
// opted in, and a static token over cleartext http is rejected unless
// explicitly overridden.
//
// Residual risk: DNS-named api_base_urls pass unchecked — resolving them
// at validation can't defeat DNS rebinding, so name-based SSRF screening
// is accepted as out of scope. Only literal IPs are denied (in any
// resolver-recognized notation: trailing-dot FQDN forms and
// inet_aton-shaped numeric hosts are refused as URLs outright, see
// urlShapeError).
func (r *ClusterRegistry) Validate(opts ValidateOptions) error {
	hostnames := map[string]struct{}{}
	ids := map[string]struct{}{}
	for _, c := range r.Clusters {
		lowerId := strings.ToLower(string(c.Id))
		if _, exists := ids[lowerId]; exists {
			return RegistryError{Kind: RegistryErrDuplicateId, Id: string(c.Id)}
		}
		ids[lowerId] = struct{}{}

		lowerHost := strings.ToLower(c.Hostname)
		if _, exists := hostnames[lowerHost]; exists {
			return RegistryError{Kind: RegistryErrDuplicateHostname, Hostname: c.Hostname}
		}
		hostnames[lowerHost] = struct{}{}

		if c.Hostname == "" || hasInvalidHostnameChar(c.Hostname) {
			return RegistryError{Kind: RegistryErrInvalidHostname, Id: string(c.Id), Hostname: c.Hostname}
		}

		isHttp := strings.HasPrefix(c.ApiBaseUrl, "http://")
		invalid := func(reason string) error {
			return RegistryError{Kind: RegistryErrInvalidUrl, Id: string(c.Id), Url: c.ApiBaseUrl, Reason: reason}
		}
		reason, authority := urlShapeError(c.ApiBaseUrl)
		if reason != "" {
			return invalid(reason)
		}
		// SSRF posture (#2/F6): literal IPs never need to name a Ray head.
		// Link-local/CGNAT ranges name cloud metadata endpoints
		// (169.254.169.254) or overlay meshes — denied outright. Loopback,
		// unspecified and RFC 1918/ULA ranges are dev-only (a local
		// `ray start --head`) — denied unless the operator opted in.
		// DNS names pass through (see the doc comment for the residual
		// risk).
		hostStr := authorityHost(authority)
		if ip := net.ParseIP(hostStr); ip != nil {
			if isDeniedSouthboundIP(ip) {
				return invalid(
					"literal IP in a link-local/CGNAT range (169.254.0.0/16, " +
						"100.64.0.0/10, fe80::/10) is not a cluster endpoint")
			}
			if !opts.AllowPrivateEndpoints && isPrivateSouthboundIP(ip) {
				return invalid(
					"literal IP in a loopback/unspecified/private range — " +
						"pass an explicit private-endpoints override for local dev")
			}
		}
		if c.AuthToken != nil && isHttp && !opts.AllowInsecureTransport {
			return RegistryError{Kind: RegistryErrCleartextToken, Id: string(c.Id)}
		}
	}
	return nil
}

// urlShapeError validates an api_base_url's scheme and authority, shared
// by Validate (static entries) and Upsert (dynamic entries). Returns a
// non-empty rejection reason, or "" plus the URL's authority.
func urlShapeError(rawurl string) (string, string) {
	if !strings.HasPrefix(rawurl, "https://") && !strings.HasPrefix(rawurl, "http://") {
		return "scheme must be http or https", ""
	}
	rest := rawurl[strings.Index(rawurl, "://")+3:]
	authority := rest
	// The authority ends at the first of '/', '?', '#'. Without the '?' cut,
	// a query glued to the host (http://169.254.169.254?x) failed ParseIP
	// and slipped a denylisted literal past the IP checks below as a "DNS
	// name", while the HTTP client still dialed the literal.
	if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
		authority = rest[:idx]
	}
	if authority == "" {
		return "missing host", ""
	}
	if strings.Contains(authority, "@") {
		return "userinfo not allowed", ""
	}
	if strings.Contains(rawurl, "#") {
		return "fragment not allowed", ""
	}
	host := authorityHost(authority)
	if host == "" {
		// http://:8265 — a port with no host. The authority is non-empty
		// so it survived the check above, but Go's dialer reads an empty
		// host as LOCALHOST: this form dialed a 127.0.0.1 listener while
		// passing every literal-IP screen (net.ParseIP("") is nil).
		// Percent-encoded hosts (%31%36%39...) collapse to "" the same
		// way once the zone-qualifier cut runs at the '%'.
		return "missing host", ""
	}
	if net.ParseIP(host) == nil {
		// Not a literal IP as far as ParseIP is concerned — it had better
		// be a real DNS name then. Two numeric notations that resolvers
		// and dialers OTHER than Go's pure-Go resolver (cgo builds,
		// dnsmasq-style upstreams) can read as IPs are refused outright:
		//
		//  - trailing-dot FQDN forms of a literal (169.254.169.254.) —
		//    many resolvers strip the root dot and return the literal;
		//  - hosts a libc inet_aton would parse as an IP (2130706433,
		//    0x7f000001, 127.1, 0251.0376.0251.0376, 169.254.43518).
		trimmed := strings.TrimRight(host, ".")
		if trimmed != host && net.ParseIP(trimmed) != nil {
			return "trailing-dot FQDN form of a literal IP is not a cluster endpoint", ""
		}
		// The inet_aton check runs on the dot-trimmed host as well: a
		// resolver that strips the root dot before parsing would read
		// 127.1. as 127.0.0.1.
		if isInetAtonShapedHost(trimmed) {
			return "host is an inet_aton-style numeric form (e.g. 2130706433, 0x7f000001, 127.1), not a DNS name", ""
		}
	}
	return "", authority
}

// isInetAtonShapedHost reports whether host parses as an IPv4 literal
// under classic libc inet_aton semantics: 1-4 dot-separated parts, each
// part decimal digits, octal (a leading 0), or hex (a 0x/0X prefix) —
// 2130706433, 0x7f000001, 0177.0.0.1 and 127.1 are all 127.0.0.1 there.
// Go's pure-Go resolver has no inet_aton semantics, but cgo-resolver
// builds and dnsmasq-style upstreams do, so such a host can dial a
// literal IP while net.ParseIP sees a DNS name; rejecting the class
// removes the dependence on which resolver the deployed binary uses.
//
// Bare hex letters without a 0x prefix are NOT numeric under inet_aton,
// so ordinary DNS names like "a", "dead.beef" or "xa" are unaffected.
func isInetAtonShapedHost(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if !isInetAtonPart(p) {
			return false
		}
	}
	return true
}

// isInetAtonPart reports whether one dot-separated part parses
// numerically under inet_aton rules: hex with a 0x/0X prefix, octal with
// a leading 0 (a part starting with 0 followed by an 8 or 9 is NOT
// numeric — glibc fails the whole parse on it), otherwise decimal.
func isInetAtonPart(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X") {
		rest := p[2:]
		if rest == "" {
			return false
		}
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
		return true
	}
	if len(p) > 1 && p[0] == '0' {
		for i := 1; i < len(p); i++ {
			if p[i] < '0' || p[i] > '7' {
				return false
			}
		}
		return true
	}
	return isAllDigits(p)
}

func hasInvalidHostnameChar(h string) bool {
	for _, ch := range h {
		if unicode.IsSpace(ch) || ch == '/' || ch == '@' || ch == '#' {
			return true
		}
	}
	return false
}

// stripPort drops a `:port` suffix from a Host header value. Bracketed
// IPv6 hosts ([::1]:8080) yield the literal inside the brackets;
// unbracketed multi-colon strings are IPv6 literals with no port to
// strip.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		rest := host[1:]
		if idx := strings.Index(rest, "]"); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}
	if strings.Count(host, ":") == 1 {
		idx := strings.LastIndex(host, ":")
		h, port := host[:idx], host[idx+1:]
		if port != "" && isAllDigits(port) {
			return h
		}
	}
	return host
}

// authorityHost extracts the host portion of a URL authority:
// [fe80::1]:8265 yields fe80::1, host:8265 yields host, host yields host.
// Userinfo is already rejected by Validate before this runs. An IPv6 zone
// qualifier (%eth0, percent-encoded or not) is stripped: it is an interface
// selector, not part of the host identity — without stripping, ParseIP
// failed on [fe80::1%25eth0] and the link-local deny was dodged.
func authorityHost(authority string) string {
	host := authority
	if strings.HasPrefix(host, "[") {
		rest := host[1:]
		if idx := strings.Index(rest, "]"); idx >= 0 {
			host = rest[:idx]
		} else {
			host = rest
		}
	} else if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	if idx := strings.IndexByte(host, '%'); idx >= 0 {
		host = host[:idx]
	}
	return host
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isDeniedSouthboundIP is the unconditional literal-IP denylist for
// southbound api_base_urls (issue #2 remainder): link-local and CGNAT
// ranges never name a Ray head — they name cloud metadata endpoints
// (169.254.169.254) or overlay meshes (Tailscale etc.). Computed from
// octets rather than net.IP's is_* helpers so the ranges are explicit and
// stable. The To4 dispatch runs first so an IPv4-mapped IPv6 literal
// (::ffff:169.254.169.254) cannot bypass the v4 ranges.
func isDeniedSouthboundIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		// 169.254.0.0/16 link-local (includes cloud metadata 169.254.169.254).
		if v4[0] == 169 && v4[1] == 254 {
			return true
		}
		// 100.64.0.0/10 CGNAT / overlay meshes.
		if v4[0] == 100 && v4[1] >= 64 && v4[1] < 128 {
			return true
		}
		return false
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return false
	}
	// fe80::/10 link-local.
	seg0 := uint16(ip16[0])<<8 | uint16(ip16[1])
	return seg0&0xffc0 == 0xfe80
}

// isPrivateSouthboundIP reports whether ip names a local or private
// endpoint (F6): loopback (127.0.0.0/8, ::1), unspecified (0.0.0.0, ::),
// RFC 1918 private ranges, or ULA (fc00::/7). Static entries carrying one
// are refused unless the operator opted in (ValidateOptions.
// AllowPrivateEndpoints); a DNS name resolving to one passes, per
// Validate's documented residual risk.
func isPrivateSouthboundIP(ip net.IP) bool {
	// IsLoopback/IsUnspecified see through IPv4-mapped IPv6 forms.
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16.
		return v4[0] == 10 ||
			(v4[0] == 172 && v4[1] >= 16 && v4[1] < 32) ||
			(v4[0] == 192 && v4[1] == 168)
	}
	if ip16 := ip.To16(); ip16 != nil {
		// fc00::/7 unique-local.
		return ip16[0]&0xfe == 0xfc
	}
	return false
}
