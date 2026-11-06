package handler

// Tests in this file exercise the WebSocket traffic contract in docs/TRAFFIC_STATS.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const wsIngressTrafficHandshakeRequestID = "wsin-handshake-request"

// wsIngressTrafficUpstream is a Responses WebSocket upstream without
// permessage-deflate, so its payload sums are the exact upstream bytes.
type wsIngressTrafficUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	turns  [][2]int64 // received, sent payload bytes per request
	// cut makes the request with this index fail after its first event.
	cut int
}

func newWSIngressTrafficUpstream(t *testing.T, cut int) *wsIngressTrafficUpstream {
	t.Helper()
	upstream := &wsIngressTrafficUpstream{cut: cut}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionDisabled})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, request, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			upstream.mu.Lock()
			index := len(upstream.turns)
			upstream.turns = append(upstream.turns, [2]int64{int64(len(request))})
			upstream.mu.Unlock()
			responseID := fmt.Sprintf("resp_ws_traffic_%d", index+1)
			text := strings.Repeat("compressible answer ", 30*(index+1))
			events := []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": responseID, "model": "gpt-5.1"}},
				{"type": "response.output_text.delta", "delta": text},
				{"type": "response.completed", "response": map[string]any{"id": responseID, "model": "gpt-5.1", "status": "completed", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}}},
			}
			if upstream.cut == -2 {
				events = []map[string]any{{"type": "error", "error": map[string]any{"type": "rate_limit_error", "code": "cyber_policy", "message": "rate limit exceeded by cyber policy"}, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}}}
			}
			for i, event := range events {
				encoded, _ := json.Marshal(event)
				if err := conn.Write(r.Context(), coderws.MessageText, encoded); err != nil {
					return
				}
				upstream.mu.Lock()
				upstream.turns[index][1] += int64(len(encoded))
				upstream.mu.Unlock()
				if index == upstream.cut && i == 0 {
					return // TCP close without a close frame or terminal event
				}
			}
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *wsIngressTrafficUpstream) turn(i int) (request, response int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.turns[i][0], u.turns[i][1]
}

type wsIngressTrafficHarness struct {
	client      *coderws.Conn
	logs        chan *service.UsageLog
	opsRepo     *trafficHandlerRepo
	handlerDone chan struct{}
	accountID   int64
	groupID     int64
}

func newWSIngressTrafficHarness(t *testing.T, upstreamURL string, compression coderws.CompressionMode) *wsIngressTrafficHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	groupID := int64(4311)
	account := service.Account{
		ID: 9961, Name: "openai-ws-ingress-traffic", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": upstreamURL},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModeCtxPool,
		},
	}
	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 4)}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		&openAIWSUsageHandlerAccountRepoStub{account: account}, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billingCacheSvc, &compositeWSHTTPUpstream{}, &service.DeferredService{},
		nil, nil, nil, nil, nil, nil, nil,
	)
	ops, opsRepo := newTrafficHandlerService(t)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := &OpenAIGatewayHandler{
		cfg:                 cfg,
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		opsService:          ops,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}
	apiKey := &service.APIKey{ID: 1861, GroupID: &groupID, User: &service.User{ID: 1761, Status: service.StatusActive}}
	handlerDone := make(chan struct{})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.RequestID, wsIngressTrafficHandshakeRequestID))
		c.Header("X-Request-Id", wsIngressTrafficHandshakeRequestID)
		c.Next()
	})
	// Production order: the traffic capture hijack excludes the handshake
	// request, and the ops middleware registers its (absent) event.
	router.Use(middleware.TrafficCapture(), OpsErrorLoggerMiddleware(ops))
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", func(c *gin.Context) {
		h.ResponsesWebSocket(c)
		close(handlerDone)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/openai/v1/responses", &coderws.DialOptions{CompressionMode: compression})
	require.NoError(t, err)
	client.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = client.CloseNow() })
	return &wsIngressTrafficHarness{client: client, logs: usageRepo.created, opsRepo: opsRepo, handlerDone: handlerDone, accountID: account.ID, groupID: groupID}
}

// turn sends one request and returns the payload bytes the client wrote and
// read until the terminal event or the end of the session.
func (h *wsIngressTrafficHarness) turn(t *testing.T, payload string) (request, response int64, terminal string) {
	t.Helper()
	writeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, h.client.Write(writeCtx, coderws.MessageText, []byte(payload)))
	cancel()
	for {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, message, err := h.client.Read(readCtx)
		cancelRead()
		if err != nil {
			return int64(len(payload)), response, ""
		}
		response += int64(len(message))
		if eventType := gjson.GetBytes(message, "type").String(); eventType == "response.completed" {
			return int64(len(payload)), response, eventType
		}
	}
}

func (h *wsIngressTrafficHarness) waitHandler(t *testing.T) {
	t.Helper()
	select {
	case <-h.handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket handler did not exit")
	}
}

func (h *wsIngressTrafficHarness) usageLogs(t *testing.T, n int) []*service.UsageLog {
	t.Helper()
	logs := make([]*service.UsageLog, 0, n)
	for len(logs) < n {
		select {
		case log := <-h.logs:
			logs = append(logs, log)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d usage logs, want %d", len(logs), n)
		}
	}
	select {
	case log := <-h.logs:
		t.Fatalf("unexpected extra usage log %s", log.RequestID)
	case <-time.After(50 * time.Millisecond):
	}
	return logs
}

// opsEvents waits for exactly n ledger events; the handshake request has none.
func (h *wsIngressTrafficHarness) opsEvents(t *testing.T, n int) []*service.OpsTrafficEvent {
	t.Helper()
	snapshot := func() []*service.OpsTrafficEvent {
		h.opsRepo.mu.Lock()
		defer h.opsRepo.mu.Unlock()
		return append([]*service.OpsTrafficEvent(nil), h.opsRepo.events...)
	}
	require.Eventually(t, func() bool { return len(snapshot()) >= n }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // a late extra event would land within the batch window
	events := snapshot()
	require.Len(t, events, n)
	return events
}

func requireWSIngressBytes(t *testing.T, m *traffic.Measurement, original, actual int64, compressed bool) {
	t.Helper()
	require.NotNil(t, m)
	require.Equal(t, compressed, m.Compressed)
	require.NotNil(t, m.OriginalBytes)
	require.NotNil(t, m.ActualBytes)
	require.Equal(t, original, *m.OriginalBytes)
	require.Equal(t, actual, *m.ActualBytes)
}

func TestOpenAIResponsesWebSocketTrafficPerTurnRowsAndLedgerEvents(t *testing.T) {
	upstream := newWSIngressTrafficUpstream(t, -1)
	harness := newWSIngressTrafficHarness(t, upstream.server.URL, coderws.CompressionDisabled)

	request1, response1, terminal1 := harness.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"input":"first turn"}`)
	require.Equal(t, "response.completed", terminal1)
	request2, response2, terminal2 := harness.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"previous_response_id":"resp_ws_traffic_1","input":"the second turn is longer"}`)
	require.Equal(t, "response.completed", terminal2)
	require.NoError(t, harness.client.Close(coderws.StatusNormalClosure, ""))
	harness.waitHandler(t)

	logs := harness.usageLogs(t, 2)
	events := harness.opsEvents(t, 2)
	byRequestID := map[string]*service.OpsTrafficEvent{}
	for _, event := range events {
		byRequestID[event.RequestID] = event
	}
	for i, want := range []struct{ request, response int64 }{{request1, response1}, {request2, response2}} {
		var log *service.UsageLog
		for _, candidate := range logs {
			if candidate.RequestID == fmt.Sprintf("resp_ws_traffic_%d", i+1) {
				log = candidate
			}
		}
		require.NotNil(t, log, "usage row of turn %d", i+1)
		require.True(t, log.OpenAIWSMode)
		require.NotNil(t, log.Traffic, "turn %d usage row must carry its snapshot", i+1)
		requireWSIngressBytes(t, log.Traffic.Downstream.Request, want.request, want.request, false)
		requireWSIngressBytes(t, log.Traffic.Downstream.Response, want.response, want.response, false)
		upstreamRequest, upstreamResponse := upstream.turn(i)
		require.NotNil(t, log.Traffic.Upstream)
		requireWSIngressBytes(t, log.Traffic.Upstream.Request, upstreamRequest, upstreamRequest, false)
		requireWSIngressBytes(t, log.Traffic.Upstream.Response, upstreamResponse, upstreamResponse, false)

		event := byRequestID[log.RequestID]
		require.NotNil(t, event, "the ledger event carries the row's request_id")
		require.Equal(t, log.Traffic, event.Traffic, "row and ledger event share one snapshot")
		require.NotNil(t, event.GroupID)
		require.Equal(t, harness.groupID, *event.GroupID)
		require.NotNil(t, event.AccountID)
		require.Equal(t, harness.accountID, *event.AccountID)
		require.False(t, event.CreatedAt.IsZero())
	}
}

func TestOpenAIResponsesWebSocketTrafficFailedTurnStillEmitsLedgerEvent(t *testing.T) {
	upstream := newWSIngressTrafficUpstream(t, 1)
	harness := newWSIngressTrafficHarness(t, upstream.server.URL, coderws.CompressionDisabled)

	_, _, terminal1 := harness.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"input":"ok"}`)
	require.Equal(t, "response.completed", terminal1)
	request2, response2, terminal2 := harness.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"input":"this turn fails"}`)
	require.Empty(t, terminal2)
	harness.waitHandler(t)

	logs := harness.usageLogs(t, 1)
	require.Equal(t, "resp_ws_traffic_1", logs[0].RequestID)
	events := harness.opsEvents(t, 2)
	var failed *service.OpsTrafficEvent
	for _, event := range events {
		if event.RequestID == logs[0].RequestID {
			require.Equal(t, logs[0].Traffic, event.Traffic)
			continue
		}
		failed = event
	}
	require.NotNil(t, failed, "a failed turn without a usage row still has its ledger event")
	require.Equal(t, wsIngressTrafficHandshakeRequestID, failed.RequestID)
	require.NotNil(t, failed.Traffic.Downstream)
	requireWSIngressBytes(t, failed.Traffic.Downstream.Request, request2, request2, false)
	requireWSIngressBytes(t, failed.Traffic.Downstream.Response, response2, response2, false)
	upstreamRequest, upstreamResponse := upstream.turn(1)
	require.NotNil(t, failed.Traffic.Upstream)
	requireWSIngressBytes(t, failed.Traffic.Upstream.Request, upstreamRequest, upstreamRequest, false)
	requireWSIngressBytes(t, failed.Traffic.Upstream.Response, upstreamResponse, upstreamResponse, false)
}

// docs/TRAFFIC_STATS.md: a turn starts only after its first complete data
// message is read; disconnecting mid-message does not create a turn event.
func TestOpenAIResponsesWebSocketTrafficIncompleteFirstMessageHasNoTurn(t *testing.T) {
	upstream := newWSIngressTrafficUpstream(t, -1)
	harness := newWSIngressTrafficHarness(t, upstream.server.URL, coderws.CompressionDisabled)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writer, err := harness.client.Writer(ctx, coderws.MessageText)
	require.NoError(t, err)
	// Exceed the websocket writer buffer to send a non-final data frame without
	// closing the message writer; closing the connection cuts off the message.
	_, err = writer.Write([]byte(`{"type":"response.create","model":"gpt-5.1","input":"` + strings.Repeat("x", 64*1024)))
	require.NoError(t, err)
	require.NoError(t, harness.client.CloseNow())
	harness.waitHandler(t)
	harness.opsEvents(t, 0)
	select {
	case log := <-harness.logs:
		t.Fatalf("incomplete first message produced usage row %s", log.RequestID)
	default:
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Empty(t, upstream.turns, "incomplete message must not reach upstream")
}

func TestOpenAIResponsesWebSocketTrafficRejectedFirstTurnEmitsLedgerEvent(t *testing.T) {
	upstream := newWSIngressTrafficUpstream(t, -1)
	harness := newWSIngressTrafficHarness(t, upstream.server.URL, coderws.CompressionDisabled)

	request, response, terminal := harness.turn(t, `{"type":"response.create","input":"no model"}`)
	require.Empty(t, terminal)
	require.Zero(t, response, "the rejection is a close frame, not a data message")
	harness.waitHandler(t)

	events := harness.opsEvents(t, 1)
	require.Equal(t, wsIngressTrafficHandshakeRequestID, events[0].RequestID)
	require.Nil(t, events[0].Traffic.Upstream, "the turn never reached an upstream")
	requireWSIngressBytes(t, events[0].Traffic.Downstream.Request, request, request, false)
	requireWSIngressBytes(t, events[0].Traffic.Downstream.Response, 0, 0, false)
	select {
	case log := <-harness.logs:
		t.Fatalf("unexpected usage row %s", log.RequestID)
	default:
	}
}

func TestOpenAIResponsesWebSocketTrafficClientDeflate(t *testing.T) {
	upstream := newWSIngressTrafficUpstream(t, -1)
	harness := newWSIngressTrafficHarness(t, upstream.server.URL, coderws.CompressionContextTakeover)

	payload := `{"type":"response.create","model":"gpt-5.1","stream":true,"input":"` + strings.Repeat("repeat ", 80) + `"}`
	request, response, terminal := harness.turn(t, payload)
	require.Equal(t, "response.completed", terminal)
	require.NoError(t, harness.client.Close(coderws.StatusNormalClosure, ""))
	harness.waitHandler(t)

	logs := harness.usageLogs(t, 1)
	events := harness.opsEvents(t, 1)
	downstream := logs[0].Traffic.Downstream
	for _, side := range []struct {
		m        *traffic.Measurement
		original int64
	}{{downstream.Request, request}, {downstream.Response, response}} {
		require.True(t, side.m.Compressed, "the client negotiated permessage-deflate")
		require.NotNil(t, side.m.OriginalBytes)
		require.Equal(t, side.original, *side.m.OriginalBytes)
		require.NotNil(t, side.m.ActualBytes)
		require.Less(t, *side.m.ActualBytes, side.original, "actual counts deflated frame payloads")
	}
	require.False(t, logs[0].Traffic.Upstream.Request.Compressed, "the upstream socket did not negotiate deflate")
	require.Equal(t, logs[0].Traffic, events[0].Traffic)
}

// docs/TRAFFIC_STATS.md: an upstream cyber-policy failure produces the same
// finalized per-turn traffic snapshot in its usage row and ledger event.
func TestOpenAIResponsesWebSocketTrafficCyberPolicyUsageMatchesLedger(t *testing.T) {
	// Use the test queue rather than starting process-global background workers.
	// Register cleanup first so the server/handler cleanups run before queue reset.
	setupOpsErrorLogTestQueue(t, 4)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
	upstream := newWSIngressTrafficUpstream(t, -2)
	harness := newWSIngressTrafficHarness(t, upstream.server.URL, coderws.CompressionDisabled)

	request, response, terminal := harness.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"input":"policy test"}`)
	require.Empty(t, terminal)
	harness.waitHandler(t)
	logs := harness.usageLogs(t, 1)
	events := harness.opsEvents(t, 1)
	// The cyber recorder enqueues after writing the usage row. Wait for that
	// goroutine before resetting the global queue in cleanup.
	select {
	case job := <-opsErrorLogQueue:
		require.NotNil(t, job.entry)
	case <-time.After(5 * time.Second):
		t.Fatal("cyber-policy error log was not enqueued")
	}
	require.Equal(t, wsIngressTrafficHandshakeRequestID, logs[0].RequestID)
	require.Equal(t, logs[0].RequestID, events[0].RequestID)
	require.NotNil(t, logs[0].Traffic)
	require.Equal(t, logs[0].Traffic, events[0].Traffic)
	requireWSIngressBytes(t, logs[0].Traffic.Downstream.Request, request, request, false)
	requireWSIngressBytes(t, logs[0].Traffic.Downstream.Response, response, response, false)
	upstreamRequest, upstreamResponse := upstream.turn(0)
	require.NotNil(t, logs[0].Traffic.Upstream)
	requireWSIngressBytes(t, logs[0].Traffic.Upstream.Request, upstreamRequest, upstreamRequest, false)
	requireWSIngressBytes(t, logs[0].Traffic.Upstream.Response, upstreamResponse, upstreamResponse, false)
}
