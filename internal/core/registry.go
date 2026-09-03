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
func (r *ClusterRegistry) Upsert(c ClusterEndpoint) error {
	if c.Hostname == "" || hasInvalidHostnameChar(c.Hostname) {
		return RegistryError{Kind: RegistryErrInvalidHostname, Id: string(c.Id), Hostname: c.Hostname}
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

// Validate validates the registry as security-sensitive input (issues
// #2/#8): duplicate hostnames/ids fail fast (first-match-wins
// misrouting), URLs are scheme-restricted with no userinfo/fragment,
// literal-IP hosts in link-local/CGNAT ranges are refused (SSRF: cloud
// metadata endpoints, overlay meshes), and a static token over cleartext
// http is rejected unless explicitly overridden.
//
// Residual risk: DNS-named api_base_urls pass unchecked — resolving them
// at validation can't defeat DNS rebinding, so name-based SSRF screening
// is accepted as out of scope. Only literal IPs are denied.
func (r *ClusterRegistry) Validate(allowInsecureTransport bool) error {
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

		isHttps := strings.HasPrefix(c.ApiBaseUrl, "https://")
		isHttp := strings.HasPrefix(c.ApiBaseUrl, "http://")
		invalid := func(reason string) error {
			return RegistryError{Kind: RegistryErrInvalidUrl, Id: string(c.Id), Url: c.ApiBaseUrl, Reason: reason}
		}
		if !isHttps && !isHttp {
			return invalid("scheme must be http or https")
		}
		rest := ""
		if idx := strings.Index(c.ApiBaseUrl, "://"); idx >= 0 {
			rest = c.ApiBaseUrl[idx+3:]
		}
		authority := rest
		if idx := strings.Index(rest, "/"); idx >= 0 {
			authority = rest[:idx]
		}
		if authority == "" {
			return invalid("missing host")
		}
		if strings.Contains(authority, "@") {
			return invalid("userinfo not allowed")
		}
		if strings.Contains(c.ApiBaseUrl, "#") {
			return invalid("fragment not allowed")
		}
		// SSRF posture (#2): literal IPs in link-local/CGNAT ranges never
		// name a Ray head — they name cloud metadata endpoints
		// (169.254.169.254) or overlay meshes. DNS names pass through
		// (see the doc comment for the residual risk).
		hostStr := authorityHost(authority)
		if ip := net.ParseIP(hostStr); ip != nil {
			if isDeniedSouthboundIP(hostStr, ip) {
				return invalid(
					"literal IP in a link-local/CGNAT range (169.254.0.0/16, " +
						"100.64.0.0/10, fe80::/10) is not a cluster endpoint")
			}
		}
		if c.AuthToken != nil && isHttp && !allowInsecureTransport {
			return RegistryError{Kind: RegistryErrCleartextToken, Id: string(c.Id)}
		}
	}
	return nil
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
// Userinfo is already rejected by Validate before this runs.
func authorityHost(authority string) string {
	if strings.HasPrefix(authority, "[") {
		rest := authority[1:]
		if idx := strings.Index(rest, "]"); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}
	if idx := strings.Index(authority, ":"); idx >= 0 {
		return authority[:idx]
	}
	return authority
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isDeniedSouthboundIP is the literal-IP denylist for southbound
// api_base_urls (issue #2 remainder): link-local and CGNAT ranges never
// name a Ray head — they name cloud metadata endpoints (169.254.169.254)
// or overlay meshes (Tailscale etc.). Computed from octets rather than
// net.IP's is_* helpers so the ranges are explicit and stable. hostStr is
// the pre-parse text form, used (like the Rust reference's string-based
// dispatch) to tell an IPv6 literal from an IPv4 one.
func isDeniedSouthboundIP(hostStr string, ip net.IP) bool {
	if strings.Contains(hostStr, ":") {
		ip16 := ip.To16()
		if ip16 == nil {
			return false
		}
		// fe80::/10 link-local.
		seg0 := uint16(ip16[0])<<8 | uint16(ip16[1])
		return seg0&0xffc0 == 0xfe80
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
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
