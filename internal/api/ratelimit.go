// Per-client-IP rate limiting for the public login route (F5).
//
// The account lockout (5 failures / 5 minutes, internal/controller's
// RecordLoginFailure) throttles per USERNAME, which an attacker can turn
// into a denial of service against any known account by spraying bad
// passwords at the public /api/v1/auth/login. This limiter throttles per
// client IP instead, so one source cannot trip account lockouts (or pay
// for unlimited bcrypt compares) at unlimited speed.
//
// stdlib-only token bucket, deliberately NOT golang.org/x/time/rate (that
// module is only an indirect dependency and no new go.mod requirement is
// wanted for twenty lines of arithmetic).
//
// In-memory and per-process: limits apply per bifrost-api replica, and
// client IP is read from the TCP peer (RemoteAddr) — never X-Forwarded-For,
// which any client can forge. A multi-replica deployment should front this
// with ingress-level limiting (the reverse proxy sees the real client IP);
// this limiter is the last-resort layer, generous enough that a single
// legitimate user behind a NAT never trips it in normal use.
package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Defaults: ~10 logins/minute sustained per IP, burst 20 — generous for
// legitimate interactive and scripted use, tight enough that holding an
// account's lockout open requires a distributed botnet rather than one
// client.
const (
	defaultLoginRatePerMinute = 10
	defaultLoginBurst         = 20
)

// loginRateLimiter is a per-client-IP token bucket. Buckets refill at
// ratePerSec and hold at most burst tokens; one request consumes one token.
type loginRateLimiter struct {
	mu         sync.Mutex
	ratePerSec float64
	burst      float64
	buckets    map[string]*loginBucket
	lastSweep  time.Time
	// now is the clock; tests substitute a fake to make refill/sweep
	// deterministic.
	now func() time.Time
}

type loginBucket struct {
	tokens float64
	last   time.Time
}

// loginBucketIdleTTL: a bucket idle this long has refilled to full, so
// dropping it loses nothing and bounds the map's growth under an IP-spray.
const loginBucketIdleTTL = 10 * time.Minute

func newLoginRateLimiter(ratePerMinute, burst int) *loginRateLimiter {
	return &loginRateLimiter{
		ratePerSec: float64(ratePerMinute) / 60,
		burst:      float64(burst),
		buckets:    map[string]*loginBucket{},
		now:        time.Now,
	}
}

// allow reports whether one request from ip may proceed.
func (l *loginRateLimiter) allow(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute {
		l.sweepLocked(now)
	}
	b, ok := l.buckets[ip]
	if !ok {
		b = &loginBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * l.ratePerSec
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked drops buckets idle past loginBucketIdleTTL. Caller holds mu.
func (l *loginRateLimiter) sweepLocked(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.last) > loginBucketIdleTTL {
			delete(l.buckets, ip)
		}
	}
	l.lastSweep = now
}

// loginBucketKey keys the bucket on the request's TCP peer — never
// X-Forwarded-For, which any client can forge. IPv4 peers key per
// address. IPv6 peers key per /64: a single IPv6 host controls its whole
// /64 prefix by convention (SLAAC/privacy extensions rotate the low 64
// bits freely), so per-address buckets would hand one machine unlimited
// fresh buckets — unlimited login attempts and unlimited account-lockout
// pressure (red-team finding). Aggregating at /64 matches how every
// rate-limiting platform treats IPv6 (Cloudflare, AWS WAF, nginx all
// default to /64-ish granularity). A zone qualifier (%eth0) and port are
// stripped first; unparseable peers key on the raw string (they still get
// ONE shared bucket, not a free pass).
func loginBucketKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port (or a bare bracketed literal): try the whole string.
		host = r.RemoteAddr
	}
	if strings.HasPrefix(host, "[") {
		if idx := strings.Index(host, "]"); idx >= 0 {
			host = host[1:idx]
		}
	}
	if idx := strings.IndexByte(host, '%'); idx >= 0 {
		host = host[:idx] // IPv6 zone is an interface selector, not identity.
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return r.RemoteAddr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// errLoginRateLimited backs the 429 the middleware emits when an IP's
// bucket is empty.
var errLoginRateLimited = HTTPError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many login attempts; try again later"}

// middleware rate-limits POST /api/v1/auth/login per client IP; every
// other request passes through untouched. NewHandler installs it inside
// the gateway dispatch, so it guards control-plane routes only.
func (l *loginRateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/login" && !l.allow(loginBucketKey(r)) {
			w.Header().Set("Retry-After", "60")
			WriteError(w, r, errLoginRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}
