package consumer

// INDEX GUARDS for the growth programmes' money paths.
//
// The Founding Family member index (one row per consumer) and the referral
// index (one referrer per referee) are built NON-fatally at boot, like the
// other phase-2 indexes, so a failed build never takes orders and wallets
// down with it. But those two indexes are what make a join and a referral
// apply exactly-once: the service's own pre-check reads "not a member yet"
// in every one of five concurrent requests, and without the index all five
// take a seat and Rs 99 (or pay a reward twice). So when the build failed,
// the two money paths refuse with a 503 rather than run on the pre-check
// alone, and the guard retries the build (at most once a minute) so a
// transient failure heals without a restart. Everything else in the
// programmes - the view, stop, the code, the ledger - keeps answering.

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// indexGuardRetryEvery spaces the rebuild attempts while an index is missing:
// often enough to heal quickly, rarely enough that a persistent failure (a
// duplicate row blocking a unique build) does not cost every request.
const indexGuardRetryEvery = time.Minute

// indexGuard records whether a uniqueness index a money path relies on is in
// place. The zero value is "built": only a failed build marks it missing.
type indexGuard struct {
	mu      sync.Mutex
	missing bool
	lastTry time.Time
	build   func(context.Context) error
}

// markMissing records a failed build and the function that retries it. The
// next ready() call retries at once.
func (g *indexGuard) markMissing(build func(context.Context) error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.missing, g.build, g.lastTry = true, build, time.Time{}
}

// ready reports whether the index is in place, retrying the build when it is
// missing and the last attempt is old enough.
func (g *indexGuard) ready(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.missing {
		return true
	}
	if g.build == nil || (!g.lastTry.IsZero() && time.Since(g.lastTry) < indexGuardRetryEvery) {
		return false
	}
	g.lastTry = time.Now()
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := g.build(bctx); err != nil {
		return false
	}
	g.missing = false
	return true
}

var (
	errFoundingUnavailable = &apiError{status: http.StatusServiceUnavailable, Code: "FOUNDING_UNAVAILABLE",
		Message: "Founding Family joins are paused for a few minutes. Nothing was charged. Please try again shortly."}
	errReferralUnavailable = &apiError{status: http.StatusServiceUnavailable, Code: "REFERRAL_UNAVAILABLE",
		Message: "Referral codes cannot be applied right now. Please try again shortly."}
)
