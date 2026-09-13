package core

import (
	"encoding/json"
	"net"
	"net/url"
	"strings"
	"testing"
)

// FuzzByHostname checks Host-header matching against a reference linear
// scan: ports ignored, case-insensitive, static wins. Invariants: no panic;
// the result agrees with stripPort + EqualFold on every input (trailing
// dots, IDN/unicode, weird bytes included), and a match always returns a
// stamped copy.
func FuzzByHostname(f *testing.F) {
	reg := &ClusterRegistry{
		Clusters: []ClusterEndpoint{
			{Id: ClusterId("demo"), Hostname: "demo.ray.example.com", ApiBaseUrl: "http://demo-head-svc:8265"},
			{Id: ClusterId("v6"), Hostname: "fe80::1", ApiBaseUrl: "http://[fe80::1]:8265"},
		},
	}
	for _, s := range []string{
		"demo.ray.example.com",
		"DEMO.ray.Example.com:8484",
		"demo.ray.example.com.", // trailing dot: must NOT match (fail closed)
		"other.example.com",
		"fe80::1",
		"[fe80::1]:8484",
		"fe80::2",
		"demo.ray.example.com:8a", // non-numeric port: not stripped
		"demo.ray.example.com:",
		"demo.ray.example.com:99999999",
		":8080",
		"",
		"\x00",
		"démo.ray.example.com",              // unicode lookalike: must NOT match
		"xn--dmo-jta.ray.example.com",       // punycode form of the above
		" DEMO.ray.example.com",             // leading space
		"demo.ray.example.com\x00.evil.com", // embedded NUL
		"[fe80::1",
		"::1",
		"[::1]:9999",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		got, found := reg.ByHostname(host)
		want := stripPort(host)
		var expect ClusterEndpoint
		expectFound := false
		for _, c := range reg.Clusters {
			if strings.EqualFold(c.Hostname, want) {
				expect, expectFound = stamp(c, RegistrySourceStatic), true
				break
			}
		}
		if found != expectFound {
			t.Fatalf("ByHostname(%q) found=%v, reference scan says %v (stripPort=%q)", host, found, expectFound, want)
		}
		if found && got != expect {
			t.Fatalf("ByHostname(%q) = %+v, want %+v", host, got, expect)
		}
	})
}

// registryWithURL builds a one-entry static registry around raw (no token,
// so only the URL checks can reject).
func registryWithURL(raw string) *ClusterRegistry {
	return &ClusterRegistry{
		Clusters: []ClusterEndpoint{
			{Id: ClusterId("c1"), Hostname: "c1.example.com", ApiBaseUrl: raw},
		},
	}
}

// FuzzRegistryURL fuzzes api_base_url validation (urlShapeError + the
// literal-IP denylists, via both Validate and Upsert). The key invariant is
// SSRF-proofing: cross-checked against net/url (the parser the HTTP client
// actually uses), any input whose real host is a denylisted literal IP must
// be rejected under EVERY option set — no alternate notation (query glue,
// zone qualifiers, v4-mapped v6) may dodge the denylist. Host forms that
// bypass net.ParseIP but that a dialer or resolver can still read as a
// literal — port-only authorities (dialed as localhost), trailing-dot FQDN
// forms, inet_aton-shaped numerics — must likewise always be rejected.
func FuzzRegistryURL(f *testing.F) {
	for _, s := range []string{
		// Valid.
		"http://demo-head-svc:8265",
		"https://demo-head-svc:8265",
		"http://c1-head-svc.ray.svc.cluster.local:8265",
		"http://172.15.255.255:8265",
		"http://192.167.1.1:8265",
		// a-f/x-only DNS names are NOT inet_aton numerics (bare hex letters
		// without a 0x prefix aren't numeric) — a charset heuristic once
		// false-positived on these.
		"http://a:8265",
		"https://x:1",
		"http://dead.beef:8265",
		"http://xa:8265",
		"http://fade:8265",
		// Shape rejections.
		"ftp://host:1",
		"https://user:pw@host:1",
		"https://host/x#frag",
		"https://",
		"not-a-url",
		"",
		// Unconditional denylist (link-local/CGNAT).
		"http://169.254.169.254:8265",
		"http://100.64.0.1:8265",
		"http://[fe80::1]:8265",
		"http://[febf::ffff]:8265",
		"http://[::ffff:169.254.169.254]:8265",
		// Fuzz-found bypasses, now regression seeds (fixed: authority is cut
		// at '?', zone qualifiers are stripped).
		"http://169.254.169.254?x",
		"http://100.64.0.1?",
		"http://[fe80::1%25eth0]:8265",
		"http://[fe80::1%eth0]:8265",
		// Private/loopback (default-deny, opt-in allow).
		"http://127.0.0.1:8265",
		"http://10.0.0.5:8265",
		"http://172.16.0.1:8265",
		"http://192.168.1.10:8265",
		"http://[::1]:8265",
		"http://[::]:8265",
		"https://[fd00::1]:8265",
		"http://[::ffff:127.0.0.1]:8265",
		// Alternate IP notations. Go's pure-Go resolver treats these as DNS
		// names, but cgo-resolver builds and dnsmasq-style upstreams read
		// them as inet_aton literals — they are now rejected outright, as
		// is the empty-host authority (dialed as localhost by Go's own
		// dialer) and trailing-dot FQDN forms of literals. The cross-check
		// below asserts the rejection.
		"http://2130706433/",
		"http://0x7f.1/",
		"http://0177.0.0.1/",
		"http://127.1/",
		"http://:8265/",
		"http://:8265",
		"http://169.254.169.254./",
		"http://127.0.0.1./",
		"http://127.1./",
		"http://2852039166/",
		"http://0xa9fea9fe/",
		"http://0251.0376.0251.0376/",
		"http://169.254.43518/",
		"http://0x7f000001/",
		"http://0/",
		"http://00.0x1/",
		// Userinfo / delimiter tricks.
		"http://host?@evil.com/",
		"http://evil.com#@169.254.169.254/",
		"http://[fe80::1%25]:8265",
		"http://%31%36%39.254.169.254/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 2048 {
			raw = raw[:2048]
		}
		reason, authority := urlShapeError(raw)
		if reason == "" && authority == "" {
			t.Fatalf("urlShapeError(%q) accepted an empty authority", raw)
		}
		if reason == "" && authorityHost(authority) == "" {
			t.Fatalf("urlShapeError(%q) accepted an empty host (dialed as localhost)", raw)
		}

		// Reference verdict from net/url: the parser the southbound HTTP
		// client actually uses. Only comparable when url.Parse accepts the
		// URL and no fragment/userinfo is involved (urlShapeError owns
		// those rejections).
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.Fragment != "" {
			return
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		host := u.Hostname()
		if i := strings.IndexByte(host, '%'); i >= 0 {
			host = host[:i]
		}

		// Unconditionally-rejected host classes. Every one of these is a
		// form some dialer or resolver can read as a literal IP (or as
		// localhost) while net.ParseIP sees nothing:
		//  - port-only authorities (http://:8265): Go's dialer reads an
		//    empty host as localhost;
		//  - trailing-dot FQDN forms of a literal (169.254.169.254.);
		//  - hosts a libc inet_aton would parse as an IP (2130706433,
		//    0x7f000001, 127.1) — checked on the dot-trimmed host too,
		//    matching urlShapeError.
		trimmed := strings.TrimRight(host, ".")
		portOnly := u.Host != "" && host == ""
		trailingDotIP := trimmed != host && net.ParseIP(trimmed) != nil
		numeric := net.ParseIP(host) == nil && isInetAtonShapedHost(trimmed)
		if portOnly || trailingDotIP || numeric {
			for _, opts := range []ValidateOptions{
				{},
				{AllowPrivateEndpoints: true},
				{AllowInsecureTransport: true},
				{AllowPrivateEndpoints: true, AllowInsecureTransport: true},
			} {
				if err := registryWithURL(raw).Validate(opts); err == nil {
					t.Fatalf("SSRF bypass: %q (host %q) accepted by Validate(%+v)", raw, host, opts)
				}
			}
			r := &ClusterRegistry{}
			if err := r.Upsert(ClusterEndpoint{
				Id: ClusterId("c1"), Hostname: "c1.example.com", ApiBaseUrl: raw,
			}); err == nil {
				t.Fatalf("SSRF bypass: %q (host %q) accepted by Upsert", raw, host)
			}
			return
		}

		ip := net.ParseIP(host)
		if ip == nil {
			return // DNS name: residual risk, documented on Validate.
		}

		if isDeniedSouthboundIP(ip) {
			// Link-local/CGNAT: rejected under every option set, and by
			// Upsert (dynamic entries have no opt-in at all).
			for _, opts := range []ValidateOptions{
				{},
				{AllowPrivateEndpoints: true},
				{AllowInsecureTransport: true},
				{AllowPrivateEndpoints: true, AllowInsecureTransport: true},
			} {
				if err := registryWithURL(raw).Validate(opts); err == nil {
					t.Fatalf("SSRF bypass: denylisted %q (host %v) accepted by Validate(%+v)", raw, ip, opts)
				}
			}
			r := &ClusterRegistry{}
			if err := r.Upsert(ClusterEndpoint{
				Id: ClusterId("c1"), Hostname: "c1.example.com", ApiBaseUrl: raw,
			}); err == nil {
				t.Fatalf("SSRF bypass: denylisted %q (host %v) accepted by Upsert", raw, ip)
			}
		}
		if isPrivateSouthboundIP(ip) {
			if err := registryWithURL(raw).Validate(ValidateOptions{}); err == nil {
				t.Fatalf("private/loopback %q (host %v) accepted without opt-in", raw, ip)
			}
			if ip.IsLoopback() || ip.IsUnspecified() {
				r := &ClusterRegistry{}
				if err := r.Upsert(ClusterEndpoint{
					Id: ClusterId("c1"), Hostname: "c1.example.com", ApiBaseUrl: raw,
				}); err == nil {
					t.Fatalf("loopback/unspecified %q (host %v) accepted by Upsert", raw, ip)
				}
			}
		}
	})
}

// FuzzStorageSourceJSON fuzzes the strict StorageSource enum decoder (the
// unmarshal path that once bricked a deployment) and the StorageEntry it
// sits in. Invariants: no panic; a failed decode never leaves a
// partially-written value behind; a successful decode always yields a known
// variant that round-trips.
func FuzzStorageSourceJSON(f *testing.F) {
	for _, s := range []string{
		`"secret"`,
		`"persistent_volume_claim"`,
		`"nfs"`,
		`"SECRET"`,
		`" secret"`,
		`"secret "`,
		`""`,
		`null`,
		`123`,
		`true`,
		`["secret"]`,
		`{"source":"secret"}`,
		`"sec\x00ret"`,
		`"secret`,
		`"persistent_volume_claim`,
		`{"name":"data","source":"secret","mode":"env","mount_path":null,"projects":[]}`,
		`{"name":"data","source":"persistent_volume_claim","claim_name":"pvc","mode":"file","mount_path":"/data","projects":["p"]}`,
		`{"name":"data","source":"nfs","mode":"env","mount_path":null,"projects":[]}`,
		`{"name":"data"}`,
		`{"source":123}`,
		`{"source":["secret"]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			data = data[:1<<16]
		}

		var src StorageSource
		before := src
		err := json.Unmarshal(data, &src)
		if err != nil {
			if src != before {
				t.Fatalf("failed decode corrupted the value: %q (input %q)", src, data)
			}
		} else {
			if !src.isValid() {
				t.Fatalf("accepted invalid StorageSource %q (input %q)", src, data)
			}
			// Round trip: marshal and decode again, must be stable.
			b, merr := json.Marshal(src)
			if merr != nil {
				t.Fatalf("marshal %q: %v", src, merr)
			}
			var back StorageSource
			if uerr := json.Unmarshal(b, &back); uerr != nil || back != src {
				t.Fatalf("round trip: %q -> %s -> %q (err %v)", src, b, back, uerr)
			}
		}

		var e StorageEntry
		if err := json.Unmarshal(data, &e); err == nil {
			if e.Source != "" && !e.Source.isValid() {
				t.Fatalf("StorageEntry accepted invalid source %q (input %q)", e.Source, data)
			}
		}
	})
}
