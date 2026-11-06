package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Tests in this file exercise the upstream WebSocket traffic contract in
// docs/TRAFFIC_STATS.md through the production forwarder and connection pool.

// wsTrafficUpstream is an in-process Responses WebSocket upstream. Its script
// returns the messages for one received request; abort closes the TCP
// connection without a close frame after those messages.
type wsTrafficUpstream struct {
	t           *testing.T
	server      *httptest.Server
	compression coderws.CompressionMode

	mu       sync.Mutex
	conns    int
	received [][]byte
	sent     [][]byte
	pings    chan chan error
	script   func(request []byte, index int) (messages [][]byte, abort bool)
}

func newWSTrafficUpstream(t *testing.T, compression coderws.CompressionMode, script func([]byte, int) ([][]byte, bool)) *wsTrafficUpstream {
	u := &wsTrafficUpstream{t: t, compression: compression, script: script, pings: make(chan chan error, 1)}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

func (u *wsTrafficUpstream) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: u.compression})
	if err != nil {
		u.t.Errorf("accept: %v", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(openAIWSMessageReadLimitBytes)
	u.mu.Lock()
	u.conns++
	u.mu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case done := <-u.pings:
				// Idle keepalive: the client's resident reader answers the pong.
				pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
				done <- conn.Ping(pingCtx)
				pingCancel()
			}
		}
	}()
	for {
		_, request, err := conn.Read(ctx)
		if err != nil {
			return
		}
		u.mu.Lock()
		index := len(u.received)
		u.received = append(u.received, request)
		u.mu.Unlock()
		messages, abort := u.script(request, index)
		for _, message := range messages {
			if err := conn.Write(ctx, coderws.MessageText, message); err != nil {
				return
			}
			u.mu.Lock()
			u.sent = append(u.sent, message)
			u.mu.Unlock()
		}
		if abort {
			_ = conn.CloseNow()
			return
		}
	}
}

func (u *wsTrafficUpstream) ping() error {
	done := make(chan error, 1)
	u.pings <- done
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("ping timed out")
	}
}

type wsTrafficMark struct{ received, sent int }

func (u *wsTrafficUpstream) mark() wsTrafficMark {
	u.mu.Lock()
	defer u.mu.Unlock()
	return wsTrafficMark{received: len(u.received), sent: len(u.sent)}
}

// since returns the payload bytes the client sent and received after mark.
func (u *wsTrafficUpstream) since(mark wsTrafficMark) (request, response int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, message := range u.received[mark.received:] {
		request += int64(len(message))
	}
	for _, message := range u.sent[mark.sent:] {
		response += int64(len(message))
	}
	return request, response
}

func (u *wsTrafficUpstream) connCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conns
}

func wsTrafficEvent(t *testing.T, value map[string]any) []byte {
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

func wsTrafficCompletedTurn(t *testing.T, responseID, text string) [][]byte {
	return [][]byte{
		wsTrafficEvent(t, map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "model": "gpt-5.1"}}),
		wsTrafficEvent(t, map[string]any{"type": "response.output_text.delta", "delta": text}),
		wsTrafficEvent(t, map[string]any{"type": "response.completed", "response": map[string]any{
			"id": responseID, "model": "gpt-5.1", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}},
			"usage":  map[string]any{"input_tokens": 3, "output_tokens": 2},
		}}),
	}
}

func newWSTrafficGatewayService(t *testing.T, upstream *wsTrafficUpstream) (*OpenAIGatewayService, *Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	pool := newOpenAIWSConnPool(cfg)
	t.Cleanup(pool.Close)
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := &Account{
		ID: 9301, Name: "openai-ws-traffic", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": upstream.server.URL},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	return svc, account
}

// forwardWSTrafficTurn sends one HTTP request through the upstream WS path and
// returns its finalized traffic.
func forwardWSTrafficTurn(t *testing.T, svc *OpenAIGatewayService, account *Account, body string) (*traffic.Usage, error) {
	t.Helper()
	collector := traffic.NewCollector()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil).WithContext(traffic.WithCollector(context.Background(), collector))
	groupID := int64(77)
	c.Set("api_key", &APIKey{GroupID: &groupID})
	_, err := svc.Forward(c.Request.Context(), c, account, []byte(body))
	collector.Finalize()
	return collector.Snapshot(), err
}

func requireWSTrafficBytes(t *testing.T, measurement *traffic.Measurement, original, actual *int64, compressed bool) {
	t.Helper()
	require.NotNil(t, measurement)
	require.Equal(t, compressed, measurement.Compressed)
	if original == nil {
		require.Nil(t, measurement.OriginalBytes)
	} else {
		require.NotNil(t, measurement.OriginalBytes)
		require.Equal(t, *original, *measurement.OriginalBytes)
	}
	if actual == nil {
		require.Nil(t, measurement.ActualBytes)
	} else {
		require.NotNil(t, measurement.ActualBytes)
		require.Equal(t, *actual, *measurement.ActualBytes)
	}
}

func int64Ref(n int64) *int64 { return &n }

func TestOpenAIWSUpstreamTrafficAttributesEachPooledTurn(t *testing.T) {
	upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func(request []byte, index int) ([][]byte, bool) {
		return wsTrafficCompletedTurn(t, fmt.Sprintf("resp_turn_%d", index+1), strings.Repeat("x", 10*(index+1))), false
	})
	svc, account := newWSTrafficGatewayService(t, upstream)

	first := upstream.mark()
	usage1, err := forwardWSTrafficTurn(t, svc, account, `{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`)
	require.NoError(t, err)
	request1, response1 := upstream.since(first)

	// Keepalive between leases belongs to no request.
	require.NoError(t, upstream.ping())

	second := upstream.mark()
	usage2, err := forwardWSTrafficTurn(t, svc, account, `{"model":"gpt-5.1","stream":false,"previous_response_id":"resp_turn_1","input":[{"type":"input_text","text":"a longer second question"}]}`)
	require.NoError(t, err)
	request2, response2 := upstream.since(second)
	require.Equal(t, 1, upstream.connCount(), "both turns must share one pooled connection")
	require.NotEqual(t, request1, request2)

	for _, turn := range []struct {
		usage             *traffic.Usage
		request, response int64
	}{{usage1, request1, response1}, {usage2, request2, response2}} {
		require.NotNil(t, turn.usage)
		require.NotNil(t, turn.usage.Upstream, "upstream WS turn must be measured")
		// Uncompressed: data-frame payload bytes equal the message payloads.
		requireWSTrafficBytes(t, turn.usage.Upstream.Request, int64Ref(turn.request), int64Ref(turn.request), false)
		requireWSTrafficBytes(t, turn.usage.Upstream.Response, int64Ref(turn.response), int64Ref(turn.response), false)
	}
}

func TestOpenAIWSUpstreamTrafficCompressionNegotiated(t *testing.T) {
	text := strings.Repeat("compressible ", 400)
	upstream := newWSTrafficUpstream(t, coderws.CompressionContextTakeover, func(request []byte, index int) ([][]byte, bool) {
		return wsTrafficCompletedTurn(t, "resp_compressed", text), false
	})
	svc, account := newWSTrafficGatewayService(t, upstream)
	mark := upstream.mark()
	usage, err := forwardWSTrafficTurn(t, svc, account, fmt.Sprintf(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":%q}]}`, text))
	require.NoError(t, err)
	request, response := upstream.since(mark)
	require.NotNil(t, usage.Upstream)
	for _, side := range []struct {
		measurement *traffic.Measurement
		original    int64
	}{{usage.Upstream.Request, request}, {usage.Upstream.Response, response}} {
		require.True(t, side.measurement.Compressed)
		require.NotNil(t, side.measurement.OriginalBytes)
		require.Equal(t, side.original, *side.measurement.OriginalBytes)
		require.NotNil(t, side.measurement.ActualBytes)
		require.Positive(t, *side.measurement.ActualBytes)
		require.Less(t, *side.measurement.ActualBytes, side.original/4, "repetitive messages must travel deflated")
		require.NotNil(t, side.measurement.Ratio())
	}
}

func TestOpenAIWSUpstreamTrafficReadFailureKeepsConsumedBytes(t *testing.T) {
	for _, compression := range []coderws.CompressionMode{coderws.CompressionDisabled, coderws.CompressionContextTakeover} {
		compressed := compression != coderws.CompressionDisabled
		t.Run(fmt.Sprintf("compressed=%v", compressed), func(t *testing.T) {
			upstream := newWSTrafficUpstream(t, compression, func(request []byte, index int) ([][]byte, bool) {
				turn := wsTrafficCompletedTurn(t, "resp_partial", strings.Repeat("partial ", 64))
				return turn[:2], true // created + delta, then the TCP connection drops
			})
			svc, account := newWSTrafficGatewayService(t, upstream)
			mark := upstream.mark()
			usage, err := forwardWSTrafficTurn(t, svc, account, `{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hello"}]}`)
			require.Error(t, err)
			request, response := upstream.since(mark)
			require.NotNil(t, usage.Upstream)
			if !compressed {
				requireWSTrafficBytes(t, usage.Upstream.Request, int64Ref(request), int64Ref(request), false)
				requireWSTrafficBytes(t, usage.Upstream.Response, int64Ref(response), int64Ref(response), false)
				return
			}
			require.True(t, usage.Upstream.Request.Compressed)
			require.Equal(t, request, *usage.Upstream.Request.OriginalBytes)
			require.NotNil(t, usage.Upstream.Request.ActualBytes)
			// Consumed compressed bytes remain; the decoded size is not paired.
			require.True(t, usage.Upstream.Response.Compressed)
			require.Nil(t, usage.Upstream.Response.OriginalBytes)
			require.NotNil(t, usage.Upstream.Response.ActualBytes)
			require.Positive(t, *usage.Upstream.Response.ActualBytes)
		})
	}
}

func TestOpenAIWSUpstreamTrafficSumsWebSocketFailureAndHTTPFallback(t *testing.T) {
	upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func(request []byte, index int) ([][]byte, bool) {
		return [][]byte{wsTrafficEvent(t, map[string]any{"type": "error", "error": map[string]any{"code": "websocket_not_supported", "message": "use HTTP"}})}, false
	})
	svc, account := newWSTrafficGatewayService(t, upstream)
	httpResponse := `{"id":"resp_http","object":"response","status":"completed"}`
	httpRequest := `{"model":"gpt-5.1","input":"fallback"}`
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, httpResponse)
	}))
	defer httpServer.Close()

	collector := traffic.NewCollector()
	ctx := traffic.WithCollector(context.Background(), collector)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil).WithContext(ctx)
	mark := upstream.mark()
	_, err := svc.Forward(ctx, c, account, []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`))
	require.Error(t, err)
	var fallback *openAIWSFallbackError
	require.ErrorAs(t, err, &fallback)
	wsRequest, wsResponse := upstream.since(mark)
	require.Positive(t, wsRequest)
	require.Positive(t, wsResponse)

	// The failover attempt on the same request uses the instrumented HTTP
	// transport and adds to the WebSocket attempt instead of replacing it.
	client := &http.Client{Transport: traffic.NewUpstreamTransport(nil)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, strings.NewReader(httpRequest))
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	collector.Finalize()
	usage := collector.Snapshot()
	require.NotNil(t, usage.Upstream)
	totalRequest := wsRequest + int64(len(httpRequest))
	totalResponse := wsResponse + int64(len(httpResponse))
	requireWSTrafficBytes(t, usage.Upstream.Request, int64Ref(totalRequest), int64Ref(totalRequest), false)
	requireWSTrafficBytes(t, usage.Upstream.Response, int64Ref(totalResponse), int64Ref(totalResponse), false)
}

func TestOpenAIWSUnmeteredConnectionKeepsUpstreamUnknown(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	pool.setClientDialerForTest(&openAIWSFakeDialer{})
	lease, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{Account: &Account{ID: 9303, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, WSURL: "ws://unused.invalid/v1/responses"})
	require.NoError(t, err)
	collector := traffic.NewCollector()
	completeUsageBody := func(direction traffic.Direction) {
		n := int64(5)
		collector.Record(direction, &n, &n, false)
	}
	completeUsageBody(traffic.UpstreamRequest) // an earlier measured HTTP attempt
	lease.beginTraffic(collector)
	lease.Release()
	completeUsageBody(traffic.UpstreamResponse)
	completeUsageBody(traffic.DownstreamResponse)
	collector.Finalize()
	usage := collector.Snapshot()
	require.Nil(t, usage.Upstream, "an unmetered WS attempt must not leave a partial upstream sum")
	require.EqualValues(t, 5, *usage.Downstream.Response.ActualBytes)
}

func TestOpenAIWSFrameMeterCountsOnlyDataPayload(t *testing.T) {
	frame := func(opcode byte, masked bool, payload int) []byte {
		out := []byte{0x80 | opcode}
		maskBit := byte(0)
		if masked {
			maskBit = 0x80
		}
		switch {
		case payload < 126:
			out = append(out, maskBit|byte(payload))
		case payload <= 0xffff:
			out = append(out, maskBit|126, byte(payload>>8), byte(payload))
		default:
			out = append(out, maskBit|127, 0, 0, 0, 0, byte(payload>>24), byte(payload>>16), byte(payload>>8), byte(payload))
		}
		if masked {
			out = append(out, 1, 2, 3, 4)
		}
		return append(out, make([]byte, payload)...)
	}
	for _, masked := range []bool{false, true} {
		var stream []byte
		stream = append(stream, frame(0x1, masked, 5)...)      // text
		stream = append(stream, frame(0x9, masked, 4)...)      // ping
		stream = append(stream, frame(0x2, masked, 300)...)    // binary, 16-bit length
		stream = append(stream, frame(0xA, masked, 0)...)      // empty pong
		stream = append(stream, frame(0x0, masked, 70000)...)  // continuation, 64-bit length
		stream = append(stream, frame(0x8, masked, 2)...)      // close
		stream = append(stream, frame(0x1, masked, 10)[:7]...) // partial text frame
		for _, chunk := range []int{1, 3, 4096, len(stream)} {
			var meter openAIWSFrameMeter
			for rest := stream; len(rest) > 0; {
				n := min(chunk, len(rest))
				meter.observe(rest[:n])
				rest = rest[n:]
			}
			consumedPartial := int64(7 - 2)
			if masked {
				consumedPartial = 7 - 6
			}
			require.Equal(t, int64(5+300+70000)+consumedPartial, meter.payload.Load(), "masked=%v chunk=%d", masked, chunk)
		}
	}
}

func TestOpenAIWSPermessageDeflateNegotiated(t *testing.T) {
	require.True(t, openAIWSPermessageDeflateNegotiated(http.Header{"Sec-Websocket-Extensions": {"permessage-deflate; client_no_context_takeover"}}))
	require.False(t, openAIWSPermessageDeflateNegotiated(http.Header{}))
	require.False(t, openAIWSPermessageDeflateNegotiated(http.Header{"Sec-Websocket-Extensions": {"x-webkit-deflate-frame"}}))
}
