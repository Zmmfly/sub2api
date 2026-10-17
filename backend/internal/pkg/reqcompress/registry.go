// Package reqcompress negotiates and applies request-body content coding for
// outbound upstream requests on a per-account basis.
//
// Providers accept different subsets of HTTP content codings for request
// entities. An account that opts in begins with an ordered trial
// (br -> zstd -> deflate); the first coding the upstream accepts is remembered
// for the lifetime of the process. A coding the upstream rejects is dropped,
// and a later rejection of the settled coding degrades the account back to
// negotiation or identity.
//
// Only the administrator's on/off switch is persisted. Negotiation results stay
// in process memory so a restart re-probes instead of trusting a stale answer.
package reqcompress

import (
	"log/slog"
	"sync"
	"time"
)

// Registry holds the per-account switch and negotiation state for one process.
type Registry struct {
	mu      sync.RWMutex
	entries map[int64]*entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[int64]*entry)}
}

// entry is the registry slot for one account. The map pointer is guarded by
// Registry.mu; the fields are guarded by entry.mu.
type entry struct {
	mu       sync.Mutex
	syncedAt time.Time
	state    *state
}

// Sync mirrors the persisted per-account switch into the registry.
//
// updatedAt orders competing snapshots: a scheduler-cache payload can lag an
// administrator edit, and an older snapshot must not re-enable an account that
// was just switched off. A zero updatedAt carries no ordering information and is
// always applied.
func (r *Registry) Sync(accountID int64, enabled bool, updatedAt time.Time) {
	r.syncObserved(accountID, &enabled, updatedAt)
}

// SyncUnknown records an observation that carries no switch value. A scheduler
// cache payload written before the switch existed has no field at all, and
// reading that absence as "off" would drop an account's negotiated state on
// every cache read, making every request probe the upstream again. Ordering
// metadata is still refreshed; the negotiated state is left untouched.
func (r *Registry) SyncUnknown(accountID int64, updatedAt time.Time) {
	r.syncObserved(accountID, nil, updatedAt)
}

// syncObserved applies one switch observation. A nil enabled value means the
// observer knew nothing about the switch, which is not the same as "disabled".
func (r *Registry) syncObserved(accountID int64, enabled *bool, updatedAt time.Time) {
	if r == nil || accountID <= 0 {
		return
	}
	slot := r.slot(accountID)
	if slot == nil {
		return
	}
	slot.mu.Lock()
	if !updatedAt.IsZero() {
		if !slot.syncedAt.IsZero() && updatedAt.Before(slot.syncedAt) {
			slot.mu.Unlock()
			return
		}
		slot.syncedAt = updatedAt
	}
	if enabled == nil {
		slot.mu.Unlock()
		return
	}
	transition := ""
	if *enabled {
		if slot.state == nil {
			slot.state = newState()
			transition = "enabled"
		}
	} else if slot.state != nil {
		// Dropping the state makes a later re-enable probe again: the upstream
		// may have changed while compression was off.
		slot.state = nil
		transition = "disabled"
	}
	slot.mu.Unlock()

	switch transition {
	case "enabled":
		slog.Debug("reqcompress switch enabled", "account_id", accountID)
	case "disabled":
		slog.Info("reqcompress switch disabled", "account_id", accountID)
	}
}

// Enabled reports whether accountID is currently opted in.
func (r *Registry) Enabled(accountID int64) bool {
	slot := r.existingSlot(accountID)
	if slot == nil {
		return false
	}
	return slot.currentState() != nil
}

func (r *Registry) slot(accountID int64) *entry {
	if r == nil || accountID <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.entries[accountID]
	if slot == nil {
		slot = &entry{}
		r.entries[accountID] = slot
	}
	return slot
}

func (r *Registry) existingSlot(accountID int64) *entry {
	if r == nil || accountID <= 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.entries[accountID]
}

func (e *entry) currentState() *state {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

var defaultRegistry = NewRegistry()

// SyncAccount mirrors one account's switch into the default registry.
func SyncAccount(accountID int64, enabled bool, updatedAt time.Time) {
	defaultRegistry.Sync(accountID, enabled, updatedAt)
}

// SyncAccountUnknown mirrors an account observation that carries no switch
// value, so an older payload cannot disable an already negotiated account.
func SyncAccountUnknown(accountID int64, updatedAt time.Time) {
	defaultRegistry.SyncUnknown(accountID, updatedAt)
}

// Enabled reports whether the default registry has compression enabled for the
// account. Unknown and disabled accounts both return false.
func Enabled(accountID int64) bool {
	return defaultRegistry.Enabled(accountID)
}
