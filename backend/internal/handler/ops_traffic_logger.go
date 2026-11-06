package handler

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const opsTrafficRegisteredKey = "ops_traffic_registered"

var opsTrafficLastEnqueueFailureLog atomic.Int64

// registerOpsTrafficEvent runs synchronously after c.Next, before any error-log
// filter. Only scalar copies escape the gin lifetime; no callback captures c,
// an API key, its group pointer, headers, or a request body.
func registerOpsTrafficEvent(c *gin.Context, ops *service.OpsService) {
	if c == nil || c.Request == nil || ops == nil || c.GetBool(opsTrafficRegisteredKey) || !ops.IsMonitoringEnabled(c.Request.Context()) {
		return
	}
	c.Set(opsTrafficRegisteredKey, true)
	ctx := c.Request.Context()
	collector := traffic.FromContext(ctx)
	apiKey := getOpsAPIKey(c)
	event := &service.OpsTrafficEvent{EventID: uuid.NewString(), Platform: resolveOpsPlatform(ctx, apiKey, "")}
	if requestID, ok := ctx.Value(ctxkey.RequestID).(string); ok {
		event.RequestID = requestID
	}
	if apiKey != nil && apiKey.GroupID != nil {
		groupID := *apiKey.GroupID
		event.GroupID = &groupID
	}
	if account, ok := c.Get(opsAccountIDKey); ok {
		if accountID, ok := account.(int64); ok && accountID > 0 {
			event.AccountID = &accountID
		}
	}
	// Do not infer an upstream platform from a URL before routing succeeded.
	event.Platform = strings.ToLower(strings.TrimSpace(event.Platform))
	traffic.AfterFinalize(ctx, func(snapshot *traffic.Usage) {
		// A WebSocket/hijacked request or a legacy route without a collector
		// has no HTTP measurement. Do not fabricate a zero-byte event; the
		// turns of a client WebSocket session have their own events.
		if snapshot == nil {
			return
		}
		// Earlier callbacks may block on usage persistence. Use the collector's
		// freeze timestamp, not this callback's wall time, for hourly buckets.
		event.CreatedAt = collector.FinalizedAt()
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		event.Traffic = snapshot
		enqueueOpsTrafficEvent(ops, event)
	})
}

// enqueueOpsTrafficEvent records one ledger event. Cancellation of the client
// must not erase the failed request, so it never uses the request context.
func enqueueOpsTrafficEvent(ops *service.OpsService, event *service.OpsTrafficEvent) {
	if err := ops.RecordTrafficEvent(context.Background(), event); err != nil {
		// Saturation may reject many requests at once. Keep every failed
		// counter, but do not turn a full telemetry queue into a log flood.
		now := time.Now().Unix()
		last := opsTrafficLastEnqueueFailureLog.Load()
		if now-last >= 30 && opsTrafficLastEnqueueFailureLog.CompareAndSwap(last, now) {
			log.Printf("[OpsTrafficLedger] request events not queued; coverage is incomplete (see traffic.ingestion)")
		}
	}
}

// opsWSTurnTrafficRecorder writes one ledger event per turn of a client
// WebSocket session: each turn is a request unit, like an HTTP request. The
// hijacked handshake request itself has no event. It holds scalar copies only,
// because turns end on relay goroutines and after the gin handler returned.
type opsWSTurnTrafficRecorder struct {
	ops       *service.OpsService
	groupID   *int64
	requestID string
	platform  atomic.Pointer[string]
}

func newOpsWSTurnTrafficRecorder(c *gin.Context, ops *service.OpsService) *opsWSTurnTrafficRecorder {
	if c == nil || c.Request == nil || ops == nil {
		return nil
	}
	recorder := &opsWSTurnTrafficRecorder{ops: ops}
	if apiKey := getOpsAPIKey(c); apiKey != nil && apiKey.GroupID != nil {
		groupID := *apiKey.GroupID
		recorder.groupID = &groupID
	}
	if requestID, ok := c.Request.Context().Value(ctxkey.RequestID).(string); ok {
		recorder.requestID = requestID
	}
	recorder.refreshPlatform(c)
	return recorder
}

// refreshPlatform resolves the platform as registerOpsTrafficEvent does, after
// routing (for example a composite decision) updated the request context.
func (r *opsWSTurnTrafficRecorder) refreshPlatform(c *gin.Context) {
	if r == nil || c == nil || c.Request == nil {
		return
	}
	platform := strings.ToLower(strings.TrimSpace(resolveOpsPlatform(c.Request.Context(), getOpsAPIKey(c), "")))
	r.platform.Store(&platform)
}

// record receives every finalized turn, including failed turns without a
// usage row. Its request ID matches the turn's usage row when there is one.
func (r *opsWSTurnTrafficRecorder) record(turn service.OpenAIWSClientTurnTraffic) {
	if r == nil || turn.Usage == nil || !r.ops.IsMonitoringEnabled(context.Background()) {
		return
	}
	event := &service.OpsTrafficEvent{EventID: uuid.NewString(), RequestID: turn.RequestID, CreatedAt: turn.FinalizedAt, Traffic: turn.Usage}
	if event.RequestID == "" {
		event.RequestID = r.requestID
	}
	if platform := r.platform.Load(); platform != nil {
		event.Platform = *platform
	}
	if r.groupID != nil {
		groupID := *r.groupID
		event.GroupID = &groupID
	}
	if turn.AccountID > 0 {
		accountID := turn.AccountID
		event.AccountID = &accountID
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	enqueueOpsTrafficEvent(r.ops, event)
}

// onTurnEnd returns the meter callback, or nil without a recorder.
func (r *opsWSTurnTrafficRecorder) onTurnEnd() func(service.OpenAIWSClientTurnTraffic) {
	if r == nil {
		return nil
	}
	return r.record
}
