package reqcompress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	sharedAlgorithmKeyPrefix = "reqcompress:account:"
	sharedStateWriteTimeout  = 5 * time.Second
	// sharedRefreshTimeout bounds the per-request TTL renewal. It is shorter
	// than sharedStateWriteTimeout because refresh is best effort and runs on
	// the request path.
	sharedRefreshTimeout = 500 * time.Millisecond
	// sharedAlgorithmTTL is a sliding lifetime: every successful negotiation and
	// every request that still uses the settled coding renews it, so only an
	// account that has been idle for the full period loses its shared value.
	sharedAlgorithmTTL = 6 * time.Hour
	// sharedInvalidationTTL bounds the retired/reset marker. After it expires a
	// stale instance could restore its coding again, so the marker only needs to
	// outlive the propagation of local state and in-flight renewals.
	sharedInvalidationTTL = 6 * time.Hour
	// sharedRefreshLogInterval limits per-account refresh failure logs so a
	// Redis outage cannot emit one warning for every request.
	sharedRefreshLogInterval = time.Minute
)

// refreshSharedAlgorithmScript atomically renews the TTL of the expected coding.
// A missing key (TTL expiry, Redis restart or flush) is recreated with the
// expected coding unless an invalidation marker (KEYS[2]) shows that the coding
// was retired or reset; a different coding published by another instance is
// left untouched, including its TTL. Return values: 1 renewed, 2 restored,
// 3 skipped because of the marker, 0 skipped because of a different coding.
var refreshSharedAlgorithmScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == ARGV[1] then
    redis.call('PEXPIRE', KEYS[1], ARGV[2])
    return 1
end
if not current then
    if redis.call('EXISTS', KEYS[2]) == 1 then
        return 3
    end
    redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
    return 2
end
return 0
`)

// persistSharedAlgorithmScript publishes a freshly negotiated coding and clears
// the invalidation marker in one step, so a later administrative re-enable can
// probe and publish again.
var persistSharedAlgorithmScript = redis.NewScript(`
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('DEL', KEYS[2])
return 1
`)

// retireSharedAlgorithmScript deletes the shared coding and leaves a
// time-limited invalidation marker. ARGV[1] is the expected coding, or empty
// for an unconditional administrative reset. A different coding currently in
// the key is neither deleted nor marked. Return values: 1 deleted, 2 key
// already absent (marker written), 0 different coding kept.
var retireSharedAlgorithmScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if ARGV[1] ~= '' and current and current ~= ARGV[1] then
    return 0
end
redis.call('DEL', KEYS[1])
redis.call('SET', KEYS[2], '1', 'PX', ARGV[2])
if current then
    return 1
end
return 2
`)

// sharedRefreshLogTimes holds the last refresh-failure log time per account
// (unix nanoseconds), bounded by the number of accounts.
var sharedRefreshLogTimes sync.Map

// ConfigureRedis attaches the process-wide registry to the shared negotiation
// store. A nil client restores the memory-only behavior used by tests and by
// deployments that have not initialized Redis yet.
func ConfigureRedis(client *redis.Client) {
	defaultRegistry.SetRedis(client)
}

// SetRedis attaches this registry to the shared negotiation store. The request
// hot path uses the process-local coding and reads Redis only while
// initializing an account; each request also renews the shared TTL on a best
// effort basis without waiting behind other renewals. Administrative display
// reads use the latest shared snapshot, and writes follow state transitions.
func (r *Registry) SetRedis(client *redis.Client) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shared = client
	r.sharedRefresh = nil
	if client != nil {
		// Cache the lightweight clone once, rather than cloning options/hooks
		// on every forwarded request. It shares (and does not own) the pool.
		r.sharedRefresh = client.WithTimeout(sharedRefreshTimeout)
	}
}

func (r *Registry) sharedRedis() *redis.Client {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.shared
}

func (r *Registry) sharedRefreshRedis() *redis.Client {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sharedRefresh
}

func sharedAlgorithmKey(accountID int64) string {
	return sharedAlgorithmKeyPrefix + strconv.FormatInt(accountID, 10) + ":algorithm"
}

// sharedInvalidationKey names the short-lived marker written when a coding is
// retired or reset. While it exists, refresh does not recreate a missing
// algorithm key, so an instance with a stale local coding cannot revive it.
func sharedInvalidationKey(accountID int64) string {
	return sharedAlgorithmKey(accountID) + ":invalidated"
}

func parseSharedAlgorithm(raw string) (Algorithm, bool) {
	algorithm := Algorithm(strings.TrimSpace(raw))
	switch algorithm {
	case AlgorithmBrotli, AlgorithmZstd, AlgorithmDeflate:
		return algorithm, true
	default:
		return AlgorithmNone, false
	}
}

// hydrateShared initializes one account from Redis. Holding the state lock
// across the read prevents a concurrent local trial from racing the snapshot.
// A failed read is retried on a later initialization attempt; the caller sends
// identity in the meantime instead of restarting negotiation with stale or
// unknown shared state.
func (r *Registry) hydrateShared(ctx context.Context, accountID int64, negotiation *state) error {
	client := r.sharedRedis()
	if client == nil || negotiation == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	negotiation.mu.Lock()
	defer negotiation.mu.Unlock()
	if negotiation.sharedLoaded || negotiation.tested || negotiation.negotiating {
		return nil
	}

	raw, err := client.Get(ctx, sharedAlgorithmKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		negotiation.sharedLoaded = true
		return nil
	}
	if err != nil {
		return err
	}
	algorithm, ok := parseSharedAlgorithm(raw)
	if !ok {
		return fmt.Errorf("invalid shared request compression algorithm %q", raw)
	}

	negotiation.sharedLoaded = true
	negotiation.current = algorithm
	negotiation.tested = true
	negotiation.negotiating = false
	negotiation.remaining = keepFrom(TrialOrder, algorithm)
	return nil
}

func sharedWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return sharedContext(ctx, sharedStateWriteTimeout)
}

func sharedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The upstream request may be canceled immediately after a successful
	// response. Shared state must still reflect that completed negotiation.
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

func (r *Registry) persistSharedAlgorithm(ctx context.Context, accountID int64, algorithm Algorithm) {
	client := r.sharedRedis()
	if client == nil || algorithm == AlgorithmNone {
		return
	}
	slot := r.existingSlot(accountID)
	if slot == nil {
		return
	}

	// Do not let a request that finishes after an administrative disable/reset
	// resurrect its old result. Reset/disable clear the local state first and
	// then delete under sharedMu, so this check and write are ordered before or
	// after them; the state lock itself is not held across the network write.
	slot.sharedMu.Lock()
	defer slot.sharedMu.Unlock()
	if negotiation := slot.currentState(); negotiation == nil || negotiation.settledAlgorithm() != algorithm {
		return
	}
	ctx, cancel := sharedWriteContext(ctx)
	defer cancel()
	// The TTL slides: requests keep renewing it through refreshSharedAlgorithm,
	// and an actual rejection retires the value and triggers renegotiation.
	if err := persistSharedAlgorithmScript.Run(ctx, client,
		[]string{sharedAlgorithmKey(accountID), sharedInvalidationKey(accountID)},
		string(algorithm), sharedAlgorithmTTL.Milliseconds()).Err(); err != nil {
		slog.Warn("reqcompress shared state store failed", "account_id", accountID,
			"algorithm", algorithm, "error", err)
	}
}

// refreshSharedAlgorithm renews the 6h shared TTL of the coding negotiation has
// settled on. Callers invoke it once per request after obtaining the state and
// before any eligibility decision, so small or ineligible requests and requests
// that later fail or are canceled still keep a probed coding alive.
//
// It is best effort and never delays other requests behind Redis I/O: the
// per-account write lock is only try-locked, so a request arriving while
// another write for the account is in flight skips its own renewal (that write
// renews the TTL or republishes the value anyway). It never changes local
// negotiation; a Redis failure is only logged (rate limited). The call is
// bounded by sharedRefreshTimeout, both through the context and through the
// client's socket deadlines, because go-redis ignores context deadlines unless
// ContextTimeoutEnabled is set.
//
// The Lua script renews only an identical value, recreates a missing key, and
// leaves a different coding untouched. Reset/disable clear the local state and
// then delete under the same write lock, so a state they replaced cannot
// republish its coding, and an in-flight renewal finishes before the delete.
func (r *Registry) refreshSharedAlgorithm(ctx context.Context, accountID int64, negotiation *state) {
	client := r.sharedRefreshRedis()
	if client == nil || negotiation == nil {
		return
	}
	slot := r.existingSlot(accountID)
	if slot == nil {
		return
	}
	if !slot.sharedMu.TryLock() {
		return
	}
	defer slot.sharedMu.Unlock()

	// Read the coding under sharedMu: a retirement changes local state before
	// it takes sharedMu to delete, so a stale coding is never republished after
	// its delete.
	if slot.currentState() != negotiation {
		return
	}
	algorithm := negotiation.settledAlgorithm()
	if algorithm == AlgorithmNone {
		return
	}
	ctx, cancel := sharedContext(ctx, sharedRefreshTimeout)
	defer cancel()
	err := refreshSharedAlgorithmScript.Run(ctx, client,
		[]string{sharedAlgorithmKey(accountID), sharedInvalidationKey(accountID)},
		string(algorithm), sharedAlgorithmTTL.Milliseconds()).Err()
	if err != nil && claimSharedRefreshLog(accountID, time.Now()) {
		slog.Warn("reqcompress shared state refresh failed", "account_id", accountID,
			"algorithm", algorithm, "error", err)
	}
}

func claimSharedRefreshLog(accountID int64, now time.Time) bool {
	value, _ := sharedRefreshLogTimes.LoadOrStore(accountID, new(atomic.Int64))
	last, ok := value.(*atomic.Int64)
	if !ok || last == nil {
		return false
	}
	nowNanos := now.UnixNano()
	for {
		previous := last.Load()
		if previous != 0 && nowNanos-previous < int64(sharedRefreshLogInterval) {
			return false
		}
		if last.CompareAndSwap(previous, nowNanos) {
			return true
		}
	}
}

// deleteSharedAlgorithm removes a coding that is no longer valid. When expected
// is non-empty, deletion is conditional so an old failure cannot remove a
// different algorithm another instance has already settled. An empty expected
// value means an administrative reset and deletes unconditionally. A delete that
// takes effect (or finds the key already gone) also leaves a 6h invalidation
// marker so stale instances cannot recreate the key; a different coding in the
// key is kept and leaves no marker.
func (r *Registry) deleteSharedAlgorithm(ctx context.Context, accountID int64, expected Algorithm) {
	client := r.sharedRedis()
	if client == nil {
		return
	}
	// Serialize with in-flight renewals and writes of this account, creating the
	// slot when needed so even an unknown account is ordered. r.mu is not held
	// while waiting for Redis.
	if slot := r.slot(accountID); slot != nil {
		slot.sharedMu.Lock()
		defer slot.sharedMu.Unlock()
	}
	ctx, cancel := sharedWriteContext(ctx)
	defer cancel()
	err := retireSharedAlgorithmScript.Run(ctx, client,
		[]string{sharedAlgorithmKey(accountID), sharedInvalidationKey(accountID)},
		string(expected), sharedInvalidationTTL.Milliseconds()).Err()
	if err != nil {
		slog.Warn("reqcompress shared state delete failed", "account_id", accountID,
			"algorithm", expected, "error", err)
	}
}

// ResetAccount drops both the local and shared negotiation result. It is used
// when the administrator toggles the switch or deletes the account; ordinary
// cache observations must not call it, or every read would force a new probe.
func ResetAccount(ctx context.Context, accountID int64) {
	defaultRegistry.Reset(ctx, accountID)
}

// Reset drops both the local and shared negotiation result for accountID.
func (r *Registry) Reset(ctx context.Context, accountID int64) {
	if r == nil || accountID <= 0 {
		return
	}
	// The slot stays in the registry: its sharedMu orders this delete with
	// in-flight Redis writes, and a replaced slot object would lose that order.
	// Clearing the state and ordering metadata is equivalent to forgetting the
	// account. Only short locks are held here; the delete waits for Redis alone.
	if slot := r.slot(accountID); slot != nil {
		slot.mu.Lock()
		slot.state = nil
		slot.syncedAt = time.Time{}
		slot.mu.Unlock()
	}
	r.deleteSharedAlgorithm(ctx, accountID, AlgorithmNone)
}

// SettledAlgorithm returns the currently pinned coding, if any. Administrative
// responses use this read-only view rather than changing the request hot path.
func SettledAlgorithm(ctx context.Context, accountID int64) Algorithm {
	return defaultRegistry.SettledAlgorithm(ctx, accountID)
}

// SettledAlgorithm returns the latest shared coding for administrative display.
// Reading Redis on each call keeps list refreshes consistent across instances;
// process-local state remains the fallback for memory-only mode and Redis errors.
func (r *Registry) SettledAlgorithm(ctx context.Context, accountID int64) Algorithm {
	slot := r.existingSlot(accountID)
	if slot == nil {
		return AlgorithmNone
	}
	negotiation := slot.currentState()
	if negotiation == nil {
		return AlgorithmNone
	}
	client := r.sharedRedis()
	if client == nil {
		return negotiation.settledAlgorithm()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := client.Get(ctx, sharedAlgorithmKey(accountID)).Result()
	if errors.Is(err, redis.Nil) {
		return AlgorithmNone
	}
	if err != nil {
		slog.Warn("reqcompress shared state load failed", "account_id", accountID, "error", err)
		return negotiation.settledAlgorithm()
	}
	algorithm, ok := parseSharedAlgorithm(raw)
	if !ok {
		slog.Warn("reqcompress shared state load failed", "account_id", accountID,
			"error", fmt.Errorf("invalid shared request compression algorithm %q", raw))
		return negotiation.settledAlgorithm()
	}
	return algorithm
}
