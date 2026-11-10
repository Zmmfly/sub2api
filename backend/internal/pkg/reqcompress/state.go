package reqcompress

import (
	"sync"
	"time"
)

const ambiguousIdentityRetryTTL = 30 * time.Minute

// state holds the process-local negotiation cache for one opted-in account.
// A zero-value state has not been negotiated yet and starts with TrialOrder.
type state struct {
	mu sync.Mutex

	// remaining lists codings that have not been proven unsupported, in
	// preference order. It narrows as trials fail.
	remaining []Algorithm
	// current is the coding used for normal requests. It is AlgorithmNone
	// until a trial succeeds and again after a settled coding is retired.
	current Algorithm
	// tested reports that negotiation finished: either current is settled or
	// every coding is known to be unsupported.
	tested bool
	// negotiating is true while one request owns the trial. Concurrent
	// requests forward identity instead of racing the upstream.
	negotiating bool
	// sharedRevision and sharedValue identify the last authoritative snapshot.
	// They fence local explicit-rejection conclusions across a shared reset.
	sharedRevision string
	sharedValue    string
	// identityRetryAt bounds a downgrade caused by ambiguous upstream failures.
	// A zero value means identity was pinned by explicit coding rejections and
	// does not expire on a timer.
	identityRetryAt time.Time
	// lastFailureLog rate-limits warning-level preservation logs so a persistent
	// upstream outage cannot flood the log while Debug still records each event.
	lastFailureLog time.Time
}

func newState() *state {
	return &state{remaining: append([]Algorithm(nil), TrialOrder...)}
}

// begin returns the codings this request should try, in order, and whether the
// caller owns negotiation bookkeeping for them.
func (s *state) begin(now time.Time, snapshots ...*sharedSnapshot) ([]Algorithm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return nil, false
	}

	if s.tested {
		if s.current != AlgorithmNone {
			return []Algorithm{s.current}, false
		}
		sharedIdentity := len(snapshots) > 0 && snapshots[0] != nil && s.sharedValue == sharedIdentityValue
		if sharedIdentity || s.identityRetryAt.IsZero() || now.Before(s.identityRetryAt) || len(s.remaining) == 0 {
			return nil, false
		}
		// Ambiguous failures only pinned identity temporarily. Re-open the
		// remaining candidates at the TTL boundary; a successful coding will
		// settle again through the ordinary negotiation path.
		s.tested = false
		s.identityRetryAt = time.Time{}
	}
	if s.negotiating {
		return nil, false
	}
	if len(s.remaining) == 0 {
		s.tested = true
		return nil, false
	}
	s.negotiating = true
	return append([]Algorithm(nil), s.remaining...), true
}

// negotiationSucceeded settles the account on the first coding the upstream
// accepted. Earlier codings are known to be unsupported and are dropped.
func (s *state) negotiationSucceeded(algorithm Algorithm, snapshots ...*sharedSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return
	}

	s.current = algorithm
	s.tested = true
	s.negotiating = false
	s.identityRetryAt = time.Time{}
	s.remaining = keepFrom(s.remaining, algorithm)
}

// negotiationCandidateFailed removes one coding the upstream rejected. When no
// candidate is left, identity lasts until reset in memory-only mode, or until
// the authoritative shared revision changes/expires with Redis.
func (s *state) negotiationCandidateFailed(algorithm Algorithm, snapshots ...*sharedSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return
	}

	s.remaining = removeAlgorithm(s.remaining, algorithm)
	if len(s.remaining) == 0 {
		s.current = AlgorithmNone
		s.tested = true
		s.negotiating = false
	}
}

// claimFailureLog returns true at most once per minute for one account, so a
// persistent failure cannot flood the warning log.
func (s *state) claimFailureLog(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.lastFailureLog.IsZero() && now.Sub(s.lastFailureLog) < time.Minute {
		return false
	}
	s.lastFailureLog = now
	return true
}

// abortNegotiation releases the trial after a transport error. The result is
// unknown, so the next request probes again instead of pinning identity.
func (s *state) abortNegotiation(snapshots ...*sharedSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return
	}

	s.negotiating = false
}

// pinIdentity finishes an exhausted initial negotiation after every coding
// was explicitly rejected. Memory-only conclusions last until reset; Redis
// conclusions remain valid only within the observed shared revision.
func (s *state) pinIdentity(snapshots ...*sharedSnapshot) {
	s.pinIdentityUntil(time.Time{}, snapshots...)
}

// pinIdentityUntil pins identity after ambiguous upstream failures. The
// remaining candidates stay intact and become eligible again at retryAt.
func (s *state) pinIdentityUntil(retryAt time.Time, snapshots ...*sharedSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return
	}

	s.current = AlgorithmNone
	s.tested = true
	s.negotiating = false
	s.identityRetryAt = retryAt
	if retryAt.IsZero() {
		s.remaining = nil
	}
}

// demoteSettled temporarily pins identity after the settled coding failed a
// request that then succeeded without content coding. It applies only while
// algorithm is still the settled coding, so a concurrent retirement,
// renegotiation or identity pin is never overwritten. (A state replaced by an
// administrative reset is detached; persistSharedIdentity then skips Redis.)
// The remaining candidates, led by the failed coding, reopen at retryAt.
func (s *state) demoteSettled(algorithm Algorithm, retryAt time.Time, snapshots ...*sharedSnapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return false
	}

	if algorithm == AlgorithmNone || !s.tested || s.negotiating || s.current != algorithm {
		return false
	}
	s.current = AlgorithmNone
	s.identityRetryAt = retryAt
	return true
}

func (s *state) identityRetry() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tested || s.current != AlgorithmNone || s.identityRetryAt.IsZero() {
		return time.Time{}, false
	}
	return s.identityRetryAt, true
}

// retireSettled handles an upstream that stopped accepting the settled coding.
// The first caller becomes the negotiation owner so it can immediately try the
// remaining codings; concurrent callers fall back to identity for one attempt.
func (s *state) retireSettled(algorithm Algorithm, snapshots ...*sharedSnapshot) ([]Algorithm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared(snapshots) {
		return nil, false
	}

	if s.current != algorithm {
		return nil, false
	}
	s.current = AlgorithmNone
	s.tested = false
	s.remaining = removeAlgorithm(s.remaining, algorithm)
	if s.negotiating || len(s.remaining) == 0 {
		if len(s.remaining) == 0 {
			s.tested = true
		}
		return nil, false
	}
	s.negotiating = true
	return append([]Algorithm(nil), s.remaining...), true
}

func keepFrom(list []Algorithm, algorithm Algorithm) []Algorithm {
	for index, candidate := range list {
		if candidate == algorithm {
			return append([]Algorithm(nil), list[index:]...)
		}
	}
	return nil
}

func removeAlgorithm(list []Algorithm, algorithm Algorithm) []Algorithm {
	out := make([]Algorithm, 0, len(list))
	for _, candidate := range list {
		if candidate != algorithm {
			out = append(out, candidate)
		}
	}
	return out
}

func (s *state) settledAlgorithm() Algorithm {
	if s == nil {
		return AlgorithmNone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tested {
		return AlgorithmNone
	}
	return s.current
}

// applyShared reconciles a complete Redis snapshot. Shared identity always
// preserves all candidates and is reopened only by the next Redis snapshot,
// never solely by a process-local elapsed timer.
func (s *state) applyShared(snapshot *sharedSnapshot, force ...bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.sharedRevision != snapshot.revision || s.sharedValue != snapshot.raw
	s.sharedRevision, s.sharedValue = snapshot.revision, snapshot.raw
	// An unchanged snapshot cannot undo a local trial/retirement in progress.
	// Publication results force reconciliation after CAS arbitration.
	if !changed && (len(force) == 0 || !force[0]) {
		return
	}
	if snapshot.raw == "" {
		if changed || s.current != AlgorithmNone || !s.identityRetryAt.IsZero() {
			s.current = AlgorithmNone
			s.tested = false
			s.negotiating = false
			s.identityRetryAt = time.Time{}
			s.remaining = append([]Algorithm(nil), TrialOrder...)
		}
		return
	}
	algorithm, _ := parseSharedAlgorithm(snapshot.raw)
	s.current = algorithm
	s.tested = true
	s.negotiating = false
	s.identityRetryAt = time.Time{}
	if algorithm == AlgorithmNone {
		s.remaining = append([]Algorithm(nil), TrialOrder...)
		// PTTL is for display/diagnostics only in shared mode. begin never
		// reopens this state until syncShared sees Redis expiry or replacement.
		s.identityRetryAt = snapshot.readAt.Add(max(snapshot.ttl, 0))
	} else {
		s.remaining = keepFrom(TrialOrder, algorithm)
	}
}

func (s *state) discardSharedResult(expected *sharedSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.matchesShared([]*sharedSnapshot{expected}) {
		return
	}
	s.current = AlgorithmNone
	s.tested = false
	s.negotiating = false
	s.identityRetryAt = time.Time{}
	s.remaining = append([]Algorithm(nil), TrialOrder...)
	s.sharedRevision = ""
	s.sharedValue = ""
}

// matchesShared fences local bookkeeping too: an older trial must not pin
// explicit identity or release a newer trial after another request saw a reset.
// The caller holds s.mu. No Redis means the original memory-only semantics.
func (s *state) matchesShared(snapshots []*sharedSnapshot) bool {
	if len(snapshots) == 0 || snapshots[0] == nil {
		return true
	}
	snapshot := snapshots[0]
	return snapshot.owner == s && s.sharedRevision == snapshot.revision && s.sharedValue == snapshot.raw
}
