# Registry api_base_url denylist dodged by query glue and IPv6 zone qualifiers

**Found:** 2026-09-12, by the new `FuzzRegistryURL` target
(`internal/core/fuzz_test.go`), whose invariant cross-checks `Validate`/
`Upsert` against `net/url` — the parser the southbound HTTP client actually
uses.

**The bug:** two ways to smuggle a denylisted literal IP past the SSRF
screens as a "DNS name":

1. `http://169.254.169.254?x` — `urlShapeError` cut the authority only at
   `/`, so the host reaching `net.ParseIP` was `169.254.169.254?x`. ParseIP
   failed, the URL passed as a DNS name, but `url.Parse(...).Hostname()` is
   `169.254.169.254`: the cloud metadata endpoint, in the unconditionally
   denied 169.254.0.0/16 range. Same for the CGNAT range
   (`http://100.64.0.1?`).
2. `http://[fe80::1%25eth0]:8265` — an IPv6 zone qualifier made ParseIP fail
   on `fe80::1%25eth0`, dodging the fe80::/10 link-local deny, while Go's
   dialer connects to `fe80::1` on zone `eth0`.

Both were accepted by `ClusterRegistry.Validate` under every option set
(including `AllowPrivateEndpoints`) and by `Upsert`.

**Fix:** `urlShapeError` now cuts the authority at the first of `/`, `?`,
`#`, and `authorityHost` strips any `%zone` suffix before the literal-IP
checks. Regression coverage: the bypass URLs are rows in
`TestValidateRejectsLinkLocalAndCgnatLiteralIps` and
`TestUpsertValidatesApiBaseUrl`, seeds in `FuzzRegistryURL`, and corpus
files under `internal/core/testdata/fuzz/FuzzRegistryURL/`.

**Not exploitable, checked anyway:** decimal/hex/octal IPv4 notations
(`http://2130706433/`, `http://0x7f.1/`, `http://0177.0.0.1/`,
`http://127.1/`) fail ParseIP but Go's resolver has no inet_aton semantics —
they are dialed as DNS names and fail resolution. Seeded into the fuzz
corpus so any future notation-aware dial path trips the cross-check.
