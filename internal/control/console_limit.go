package control

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// B5.1: the console's rate limit.
//
// B5 shipped with none, which matters less for abuse than for accidents: the
// console's read endpoints run unbounded scans over usage_ledger and
// ClickHouse, and a retry loop or a refresh-happy browser tab can put control's
// database under load that gateway's hot path then shares. A tenant cannot
// reach this listener at all, so the threat being managed is an operator's
// tooling, not an attacker.
//
// That framing sets the shape: a small token bucket, per capability level
// rather than per caller, refusing with 429 and a Retry-After. It is
// deliberately not the gateway's limiter -- that one is distributed through
// Redis because gateway is replicated and its limits are a product promise.
// This is one process protecting one database from one operator's script, so
// an in-process bucket is the honest scope, and saying so here stops someone
// later assuming the console is globally limited when it is per instance.
const (
	// consoleRateBurst is how many requests may arrive at once.
	consoleRateBurst = 30
	// consoleRateRefill is how fast the bucket refills, in requests/second.
	// 5/s sustained is far above interactive use and far below what it takes
	// to hurt PostgreSQL with the heaviest endpoint here.
	consoleRateRefill = 5.0
)

// consoleLimiter is a token bucket. Writes and reads get separate buckets:
// a runaway dashboard polling GET /v1/accounts must not be able to exhaust the
// budget that an operator needs to resolve a held row.
type consoleLimiter struct {
	mu      sync.Mutex
	tokens  map[consoleAccess]float64
	updated map[consoleAccess]time.Time
	burst   float64
	refill  float64
	now     func() time.Time
}

func newConsoleLimiter(burst, refill float64, now func() time.Time) *consoleLimiter {
	if now == nil {
		now = time.Now
	}
	return &consoleLimiter{
		tokens:  map[consoleAccess]float64{},
		updated: map[consoleAccess]time.Time{},
		burst:   burst,
		refill:  refill,
		now:     now,
	}
}

// allow takes one token from the bucket for this kind of request, reporting
// whether it was available and how long until the next one is.
func (l *consoleLimiter) allow(kind consoleAccess) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	last, seen := l.updated[kind]
	if !seen {
		// A bucket starts full: the first request after a restart must not be
		// the one that gets refused.
		l.tokens[kind] = l.burst
	} else if elapsed := now.Sub(last).Seconds(); elapsed > 0 {
		l.tokens[kind] = min(l.burst, l.tokens[kind]+elapsed*l.refill)
	}
	l.updated[kind] = now
	if l.tokens[kind] < 1 {
		// Report when one whole token will exist, rounded up, so a caller that
		// honours Retry-After is not immediately refused again.
		missing := 1 - l.tokens[kind]
		wait := time.Duration(missing / l.refill * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	l.tokens[kind]--
	return true, 0
}

// rateLimit wraps the mux. It runs inside authorize, so an unauthenticated
// flood is rejected by the cheaper check first and never consumes a token --
// otherwise anyone who could reach the port could deny the console to the
// operator who holds a real credential.
func (c Console) rateLimit(next http.Handler) http.Handler {
	limiter := c.Limiter
	if limiter == nil {
		limiter = newConsoleLimiter(consoleRateBurst, consoleRateRefill, c.Now)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := consoleReadOnly
		if r.Method != http.MethodGet {
			kind = consoleReadWrite
		}
		allowed, wait := limiter.allow(kind)
		if !allowed {
			c.metrics().AddCounter("control_console_throttled_total", 1, "path", r.URL.Path)
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.5)))
			writeError(w, http.StatusTooManyRequests, "console rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
