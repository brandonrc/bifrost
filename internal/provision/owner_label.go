package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// OwnerRawAnnotation carries the unstamped identity (OIDC
// preferred_username/sub) next to the sanitized [OwnerLabel] value, so the
// raw owner stays observable when the label had to be rewritten (F9).
// Stamped on the RayCluster/RayJob metadata (pods inherit the label).
const OwnerRawAnnotation = "bifrost.dev/owner-raw"

// ownerLabelValue maps an identity Owner() string to a value the
// API server accepts as a Kubernetes label value (RFC 1123 label, ≤63
// chars). IdP usernames are arbitrary — emails, spaces, mixed case, 200-char
// subs — and stamping one verbatim wedges every server-side apply for that
// user (F9).
//
// An owner that already is a valid label value passes through unchanged:
// the per-owner NetworkPolicy selector and the pod/CR labels all route
// through this one function, so identical inputs keep matching. Anything
// needing a rewrite (invalid chars, uppercase, overlong) also gets a
// `-<16 hex of sha256(raw)>` suffix, so two owners that differ only in
// characters the mapping erases (jane@x.com vs jane#x.com) never collide
// on the same label.
//
// The suffix is 64 bits of the SHA-256 of the RAW owner. Collision
// resistance is a security property here, not a convenience: a rewritten
// owner's label selects the per-owner NetworkPolicy pin, and an attacker
// whose IdP hands them a controlled preferred_username could otherwise
// grind a preimage of a victim's suffix (a 32-bit suffix falls in minutes
// at ~5.5 Mhash/s single-core) and reach the victim's Ray client port.
// 64 bits puts that out of brute-force reach; it assumes SHA-256's
// preimage resistance, nothing about the owner's entropy.
//
// Deployment requirement: the IdP must guarantee preferred_username is
// unique per user and immutable. Two users sharing one username share one
// owner label and therefore one NetworkPolicy pin, regardless of the hash
// suffix; a mutable username lets an attacker rename themselves onto a
// victim's un-rewritten label directly.
func ownerLabelValue(owner string) string {
	var b strings.Builder
	b.Grow(len(owner))
	for _, r := range owner {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	mapped := strings.Trim(b.String(), "-")
	if mapped == owner && len(owner) <= 63 {
		return owner
	}
	sum := sha256.Sum256([]byte(owner))
	suffix := hex.EncodeToString(sum[:])[:16]
	// 63 total, "-" + 16 hex = 17 reserved for the suffix.
	const maxStem = 63 - 17
	if len(mapped) > maxStem {
		mapped = strings.TrimRight(mapped[:maxStem], "-")
	}
	if mapped == "" {
		mapped = "owner"
	}
	return mapped + "-" + suffix
}
