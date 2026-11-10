package reqcompress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	sharedIdentityValue      = "identity"
	sharedStateWriteTimeout  = 5 * time.Second
	sharedRefreshTimeout     = 500 * time.Millisecond
	sharedAlgorithmTTL       = 6 * time.Hour
	sharedInvalidationTTL    = 6 * time.Hour
	sharedRefreshLogInterval = time.Minute
)

// Each state transition rotates a bounded revision token. The algorithm key
// stays compatible with older releases, but all instances must be upgraded to
// enforce fencing: older writers do not compare revisions. Revisions outlive
// the 30-minute identity key and are initialized again after expiry/Redis restart,
// so a missing key never lets a stale request revive an old cooldown or coding.
// All scripts return value, PTTL and revision from the same atomic operation.
const sharedSnapshotLua = `
local function snapshot()
    return {redis.call('GET', KEYS[1]) or '', redis.call('PTTL', KEYS[1]), redis.call('GET', KEYS[3]) or ''}
end
`

var readSharedAlgorithmScript = redis.NewScript(sharedSnapshotLua + `
local revision = redis.call('GET', KEYS[3])
if not revision then
    redis.call('SET', KEYS[3], ARGV[1], 'PX', ARGV[2])
end
local current = redis.call('GET', KEYS[1])
if current == 'br' or current == 'zstd' or current == 'deflate' then
    redis.call('PEXPIRE', KEYS[1], ARGV[2])
    redis.call('PEXPIRE', KEYS[3], ARGV[2])
elseif current == 'identity' and redis.call('PTTL', KEYS[1]) < 0 then
    -- A legacy non-expiring identity must not disable recovery forever.
    redis.call('DEL', KEYS[1])
    redis.call('SET', KEYS[3], ARGV[1], 'PX', ARGV[2])
end
return snapshot()
`)

var persistSharedAlgorithmScript = redis.NewScript(sharedSnapshotLua + `
local current = redis.call('GET', KEYS[1]) or ''
local revision = redis.call('GET', KEYS[3]) or ''
if revision == ARGV[1] and current == ARGV[2] and current ~= 'identity' then
    redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
    redis.call('SET', KEYS[3], ARGV[5], 'PX', ARGV[4])
    redis.call('DEL', KEYS[2])
end
return snapshot()
`)

// A settled replay may only replace the coding it actually observed. In
// particular, an expired/missing coding is not permission to create identity.
// Fresh trials can publish from an observed missing key. A concurrent identity
// or coding wins, and returning its PTTL aligns the losing instance immediately.
var persistSharedIdentityScript = redis.NewScript(sharedSnapshotLua + `
local current = redis.call('GET', KEYS[1]) or ''
local revision = redis.call('GET', KEYS[3]) or ''
if revision == ARGV[1] and current == ARGV[2] and current ~= 'identity'
   and current == ARGV[3] then
    redis.call('SET', KEYS[1], 'identity', 'PX', ARGV[4])
    redis.call('SET', KEYS[3], ARGV[5], 'PX', ARGV[6])
    redis.call('DEL', KEYS[2])
end
return snapshot()
`)

var retireSharedAlgorithmScript = redis.NewScript(sharedSnapshotLua + `
local current = redis.call('GET', KEYS[1]) or ''
local revision = redis.call('GET', KEYS[3]) or ''
if ARGV[1] == '' or (current == ARGV[1] and revision == ARGV[2]) then
    redis.call('DEL', KEYS[1])
    redis.call('SET', KEYS[2], '1', 'PX', ARGV[3])
    redis.call('SET', KEYS[3], ARGV[4], 'PX', ARGV[3])
end
return snapshot()
`)

// sharedScripter disables transport retries for these stateful scripts. In
// particular an unconditional reset must not execute again after a lost reply
// and delete a newer instance's publication. redis.Script.Run may still retry
// NOSCRIPT with EVAL, because NOSCRIPT guarantees no script was executed.
type sharedScripter struct{ *redis.Client }
type sharedCommand struct{ *redis.Cmd }

func (*sharedCommand) NoRetry() bool { return true }
func (c sharedScripter) eval(ctx context.Context, operation, script string, keys []string, args ...any) *redis.Cmd {
	commandArgs := []any{operation, script, len(keys)}
	for _, key := range keys {
		commandArgs = append(commandArgs, key)
	}
	commandArgs = append(commandArgs, args...)
	cmd := redis.NewCmd(ctx, commandArgs...)
	_ = c.Process(ctx, &sharedCommand{Cmd: cmd})
	return cmd
}
func (c sharedScripter) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	return c.eval(ctx, "eval", script, keys, args...)
}
func (c sharedScripter) EvalSha(ctx context.Context, hash string, keys []string, args ...any) *redis.Cmd {
	return c.eval(ctx, "evalsha", hash, keys, args...)
}

var sharedRefreshLogTimes sync.Map
var errSharedStateBusy = errors.New("request compression shared state is being updated")

// sharedSnapshot is owned by one Execute call. Only that request's successful
// retirement advances it, allowing its replacement trial to publish. Conflicts
// never advance the expected revision, even when local state is reconciled.
type sharedSnapshot struct {
	owner    *state
	raw      string
	revision string
	ttl      time.Duration
	readAt   time.Time
}

func newSharedRevision() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic("reqcompress: cannot generate shared revision: " + err.Error())
	}
	return hex.EncodeToString(token[:])
}

func sharedKeys(accountID int64) []string {
	return []string{sharedAlgorithmKey(accountID), sharedInvalidationKey(accountID), sharedRevisionKey(accountID)}
}

func decodeSharedSnapshot(result any, readAt time.Time) (*sharedSnapshot, error) {
	values, ok := result.([]any)
	if !ok || len(values) != 3 {
		return nil, fmt.Errorf("invalid shared request compression snapshot %T", result)
	}
	raw, rawOK := values[0].(string)
	ttl, ttlOK := values[1].(int64)
	revision, revisionOK := values[2].(string)
	if !rawOK || !ttlOK || !revisionOK {
		return nil, fmt.Errorf("invalid shared request compression snapshot fields")
	}
	if raw != "" {
		if _, ok := parseSharedAlgorithm(raw); !ok {
			return nil, fmt.Errorf("invalid shared request compression algorithm %q", raw)
		}
	}
	return &sharedSnapshot{raw: raw, revision: revision, ttl: time.Duration(ttl) * time.Millisecond, readAt: readAt}, nil
}

func ConfigureRedis(client *redis.Client) { defaultRegistry.SetRedis(client) }

// SetRedis configures the authoritative shared store. Each request reads its
// current value and renews only a coding's sliding TTL. Redis errors or a busy
// local writer send this request plain, without creating a local cooldown;
// the next request retries Redis. Memory-only deployments keep local behavior.
func (r *Registry) SetRedis(client *redis.Client) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shared = client
	r.sharedRefresh = nil
	r.sharedWrite = nil
	if client != nil {
		r.sharedRefresh = client.WithTimeout(sharedRefreshTimeout)
		r.sharedWrite = client.WithTimeout(sharedStateWriteTimeout)
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
func (r *Registry) sharedWriteRedis() *redis.Client {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sharedWrite
}
func sharedAlgorithmKey(accountID int64) string {
	return sharedAlgorithmKeyPrefix + strconv.FormatInt(accountID, 10) + ":algorithm"
}
func sharedInvalidationKey(accountID int64) string {
	return sharedAlgorithmKey(accountID) + ":invalidated"
}
func sharedRevisionKey(accountID int64) string { return sharedAlgorithmKey(accountID) + ":revision" }
func parseSharedAlgorithm(raw string) (Algorithm, bool) {
	algorithm := Algorithm(strings.TrimSpace(raw))
	switch algorithm {
	case AlgorithmBrotli, AlgorithmZstd, AlgorithmDeflate:
		return algorithm, true
	case sharedIdentityValue:
		return AlgorithmNone, true
	default:
		return AlgorithmNone, false
	}
}

// sharedReadFlight shares one bounded read among overlapping requests. Each
// waiter gets its own snapshot copy because a successful request retirement
// advances only that request's expected revision.
type sharedReadFlight struct {
	owner    *state
	done     chan struct{}
	snapshot *sharedSnapshot
	err      error
}

// syncShared synchronizes every request, including already settled instances.
// Concurrent readers share one Redis call instead of losing compression during
// normal bursts. A writer already holding sharedMu still fails open to plain.
func (r *Registry) syncShared(ctx context.Context, accountID int64, negotiation *state) (*sharedSnapshot, error) {
	client := r.sharedRefreshRedis()
	if client == nil || negotiation == nil {
		return nil, nil
	}
	slot := r.existingSlot(accountID)
	if slot == nil {
		return nil, errSharedStateBusy
	}
	slot.mu.Lock()
	if slot.state != negotiation {
		slot.mu.Unlock()
		return nil, errSharedStateBusy
	}
	flight := slot.sharedRead
	leader := flight == nil || flight.owner != negotiation
	if leader {
		flight = &sharedReadFlight{owner: negotiation, done: make(chan struct{})}
		slot.sharedRead = flight
	}
	slot.mu.Unlock()
	if leader {
		flight.snapshot, flight.err = r.readShared(ctx, client, slot, accountID, negotiation)
		slot.mu.Lock()
		if slot.sharedRead == flight {
			slot.sharedRead = nil
		}
		close(flight.done)
		slot.mu.Unlock()
	} else {
		waitCtx, cancel := sharedContext(ctx, sharedRefreshTimeout)
		defer cancel()
		select {
		case <-flight.done:
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		}
	}
	if flight.err != nil {
		return nil, flight.err
	}
	if slot.currentState() != negotiation {
		return nil, errSharedStateBusy
	}
	snapshot := *flight.snapshot
	return &snapshot, nil
}

func (r *Registry) readShared(ctx context.Context, client *redis.Client, slot *entry, accountID int64, negotiation *state) (*sharedSnapshot, error) {
	if !slot.sharedMu.TryLock() {
		return nil, errSharedStateBusy
	}
	defer slot.sharedMu.Unlock()
	if slot.currentState() != negotiation {
		return nil, errSharedStateBusy
	}
	ctx, cancel := sharedContext(ctx, sharedRefreshTimeout)
	defer cancel()
	// Start before the round trip: a delayed reply must never move an identity
	// deadline later than Redis's actual remaining lifetime.
	started := r.now()
	result, err := readSharedAlgorithmScript.Run(ctx, sharedScripter{client}, sharedKeys(accountID), newSharedRevision(), sharedAlgorithmTTL.Milliseconds()).Result()
	if err != nil {
		return nil, err
	}
	snapshot, err := decodeSharedSnapshot(result, started)
	if err != nil {
		return nil, err
	}
	if slot.currentState() != negotiation {
		return nil, errSharedStateBusy
	}
	snapshot.owner = negotiation
	negotiation.applyShared(snapshot)
	return snapshot, nil
}

func sharedWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return sharedContext(ctx, sharedStateWriteTimeout)
}
func sharedContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// persistSharedAlgorithm publishes only against the snapshot that started the
// request. A late success cannot replace a cooldown, a newer coding, or reset.
func (r *Registry) persistSharedAlgorithm(ctx context.Context, accountID int64, algorithm Algorithm, expected *sharedSnapshot) {
	client := r.sharedWriteRedis()
	if client == nil || algorithm == AlgorithmNone || expected == nil {
		return
	}
	slot := r.existingSlot(accountID)
	if slot == nil {
		return
	}
	slot.sharedMu.Lock()
	defer slot.sharedMu.Unlock()
	negotiation := slot.currentState()
	if negotiation == nil || negotiation != expected.owner {
		return
	}
	ctx, cancel := sharedWriteContext(ctx)
	defer cancel()
	started := r.now()
	result, err := persistSharedAlgorithmScript.Run(ctx, sharedScripter{client}, sharedKeys(accountID), expected.revision, expected.raw,
		string(algorithm), sharedAlgorithmTTL.Milliseconds(), newSharedRevision()).Result()
	r.applySharedWrite(slot, negotiation, accountID, expected, started, result, err)
}

func (r *Registry) persistSharedIdentity(ctx context.Context, accountID int64, negotiation *state, _ time.Time, replaceable Algorithm, expected *sharedSnapshot) {
	client := r.sharedWriteRedis()
	if client == nil || negotiation == nil || expected == nil {
		return
	}
	slot := r.existingSlot(accountID)
	if slot == nil {
		return
	}
	slot.sharedMu.Lock()
	defer slot.sharedMu.Unlock()
	if slot.currentState() != negotiation {
		return
	}
	// The Redis command creates the fixed deadline, not a process-local timer.
	// Do not retry this write after an uncertain outcome; next request reads it.
	ctx, cancel := sharedWriteContext(ctx)
	defer cancel()
	started := r.now()
	result, err := persistSharedIdentityScript.Run(ctx, sharedScripter{client}, sharedKeys(accountID), expected.revision, expected.raw,
		string(replaceable), ambiguousIdentityRetryTTL.Milliseconds(), newSharedRevision(), sharedInvalidationTTL.Milliseconds()).Result()
	r.applySharedWrite(slot, negotiation, accountID, expected, started, result, err)
}

func (r *Registry) applySharedWrite(slot *entry, negotiation *state, accountID int64, expected *sharedSnapshot, started time.Time, result any, err error) {
	if slot.currentState() != negotiation {
		return
	}
	var snapshot *sharedSnapshot
	if err == nil {
		snapshot, err = decodeSharedSnapshot(result, started)
	}
	if err != nil {
		// A failed/uncertain publication is not a local capability conclusion.
		// Never retain its independent 30-minute timer or permanent identity.
		negotiation.discardSharedResult(expected)
		if claimSharedRefreshLog(accountID, time.Now()) {
			slog.Warn("reqcompress shared state store failed", "account_id", accountID, "error", err)
		}
		return
	}
	negotiation.applyShared(snapshot, true)
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

// deleteSharedAlgorithm is unconditional only for administrative reset/disable.
// Request retirements compare the exact observed coding and revision. A winning
// retirement advances its request snapshot for the replacement negotiation.
func (r *Registry) deleteSharedAlgorithm(ctx context.Context, accountID int64, expected Algorithm, snapshots ...*sharedSnapshot) {
	client := r.sharedWriteRedis()
	if client == nil {
		return
	}
	slot := r.slot(accountID)
	slot.sharedMu.Lock()
	defer slot.sharedMu.Unlock()
	var before *sharedSnapshot
	if len(snapshots) != 0 {
		before = snapshots[0]
	}
	if expected != AlgorithmNone && (before == nil || slot.currentState() != before.owner) {
		return
	}
	revision := ""
	if before != nil {
		revision = before.revision
	}
	token := newSharedRevision()
	ctx, cancel := sharedWriteContext(ctx)
	defer cancel()
	started := r.now()
	result, err := retireSharedAlgorithmScript.Run(ctx, sharedScripter{client}, sharedKeys(accountID), string(expected), revision,
		sharedInvalidationTTL.Milliseconds(), token).Result()
	var snapshot *sharedSnapshot
	if err == nil {
		snapshot, err = decodeSharedSnapshot(result, started)
	}
	if err != nil {
		if before != nil && slot.currentState() == before.owner {
			before.owner.discardSharedResult(before)
		}
		slog.Warn("reqcompress shared state delete failed", "account_id", accountID, "algorithm", expected, "error", err)
		return
	}
	if before != nil && snapshot.revision == token {
		snapshot.owner = before.owner
		*before = *snapshot
		if negotiation := slot.currentState(); negotiation == before.owner {
			negotiation.mu.Lock()
			negotiation.sharedRevision, negotiation.sharedValue = snapshot.revision, snapshot.raw
			negotiation.mu.Unlock()
		}
	} else if expected != AlgorithmNone {
		if negotiation := slot.currentState(); negotiation == before.owner {
			negotiation.applyShared(snapshot, true)
		}
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
