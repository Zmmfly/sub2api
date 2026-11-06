package service

// Tests in this file exercise the WebSocket traffic contract in docs/TRAFFIC_STATS.md.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// wsClientTrafficIngress runs ProxyResponsesWebSocketFromClient behind a real
// client WebSocket, metered the way the Responses WebSocket handler meters it.
type wsClientTrafficIngress struct {
	server *httptest.Server
	mu     sync.Mutex
	turns  []OpenAIWSClientTurnTraffic
	done   chan error
}

func startWSClientTrafficIngress(t *testing.T, svc *OpenAIGatewayService, account *Account) *wsClientTrafficIngress {
	t.Helper()
	controlCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ingress := &wsClientTrafficIngress{done: make(chan error, 1)}
	ingress.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meter := NewOpenAIWSClientTraffic()
		meter.SetOnTurnEnd(func(turn OpenAIWSClientTurnTraffic) {
			ingress.mu.Lock()
			ingress.turns = append(ingress.turns, turn)
			ingress.mu.Unlock()
		})
		ginCtx, _ := gin.CreateTestContext(w)
		ginCtx.Request = r
		conn, err := coderws.Accept(meter.WrapResponseWriter(ginCtx.Writer), r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			ingress.done <- err
			return
		}
		conn.SetReadLimit(openAIWSMessageReadLimitBytes)
		ctx := WithOpenAIWSClientTraffic(controlCtx, meter)
		ginCtx.Request = r.Clone(ctx)
		_, first, err := meter.ReadClientMessage(ctx, conn, 3*time.Second, coderws.StatusPolicyViolation, "missing first message")
		if err != nil {
			meter.Close()
			ingress.done <- err
			return
		}
		meter.BeginTurn()
		err = svc.ProxyResponsesWebSocketFromClient(ctx, ginCtx, conn, account, "sk-test", first, nil)
		meter.Close()
		_ = conn.CloseNow()
		ingress.done <- err
	}))
	t.Cleanup(ingress.server.Close)
	return ingress
}

func (i *wsClientTrafficIngress) turnCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.turns)
}

func (i *wsClientTrafficIngress) waitTurns(t *testing.T, n int) []OpenAIWSClientTurnTraffic {
	t.Helper()
	require.Eventually(t, func() bool { return i.turnCount() >= n }, 5*time.Second, 5*time.Millisecond)
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]OpenAIWSClientTurnTraffic(nil), i.turns...)
}

func (i *wsClientTrafficIngress) waitDone(t *testing.T) error {
	t.Helper()
	select {
	case err := <-i.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("ingress did not finish")
		return nil
	}
}

// wsClientTrafficClient is the test client. Its wire counter observes the
// client side of the same frames, so it gives the expected actual bytes even
// with permessage-deflate.
type wsClientTrafficClient struct {
	conn *coderws.Conn
	wire *openAIWSWireCounter
}

func dialWSClientTraffic(t *testing.T, ingress *wsClientTrafficIngress, compression coderws.CompressionMode) *wsClientTrafficClient {
	t.Helper()
	httpClient, transport := newOpenAIWSWireCountingClient(nil)
	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(ingress.server.URL, "http"), &coderws.DialOptions{HTTPClient: httpClient, CompressionMode: compression})
	require.NoError(t, err)
	conn.SetReadLimit(openAIWSMessageReadLimitBytes)
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.NotNil(t, transport.upgraded())
	return &wsClientTrafficClient{conn: conn, wire: transport.upgraded()}
}

// wsClientTurnBytes is what the client observed for one turn.
type wsClientTurnBytes struct {
	requestOriginal, requestActual   int64
	responseOriginal, responseActual int64
	terminal                         string
}

// turn writes one request and reads events until a terminal event or a read
// error. It measures the client side of the turn.
func (c *wsClientTrafficClient) turn(t *testing.T, payload string) wsClientTurnBytes {
	t.Helper()
	written, read := c.wire.written.payload.Load(), c.wire.read.payload.Load()
	writeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, c.conn.Write(writeCtx, coderws.MessageText, []byte(payload)))
	cancel()
	got := wsClientTurnBytes{requestOriginal: int64(len(payload))}
	for {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
		_, message, err := c.conn.Read(readCtx)
		cancelRead()
		if err != nil {
			break
		}
		got.responseOriginal += int64(len(message))
		if eventType := gjson.GetBytes(message, "type").String(); isOpenAIWSTerminalEvent(eventType) {
			got.terminal = eventType
			break
		}
	}
	got.requestActual = c.wire.written.payload.Load() - written
	got.responseActual = c.wire.read.payload.Load() - read
	return got
}

func requireWSClientTurnDownstream(t *testing.T, usage *traffic.Usage, want wsClientTurnBytes, compressed bool) {
	t.Helper()
	require.NotNil(t, usage)
	require.NotNil(t, usage.Downstream)
	requireWSTrafficBytes(t, usage.Downstream.Request, int64Ref(want.requestOriginal), int64Ref(want.requestActual), compressed)
	requireWSTrafficBytes(t, usage.Downstream.Response, int64Ref(want.responseOriginal), int64Ref(want.responseActual), compressed)
}

func TestOpenAIWSClientTrafficPooledModesTurnsAreSeparate(t *testing.T) {
	// docs/TRAFFIC_STATS.md: ctx_pool, shared and dedicated all meter each turn.
	for _, tc := range []struct {
		name        string
		mode        string
		compression coderws.CompressionMode
		compressed  bool
	}{
		{name: "ctx_pool/identity", mode: OpenAIWSIngressModeCtxPool, compression: coderws.CompressionDisabled},
		{name: "ctx_pool/permessage_deflate", mode: OpenAIWSIngressModeCtxPool, compression: coderws.CompressionContextTakeover, compressed: true},
		{name: "shared/identity", mode: OpenAIWSIngressModeShared, compression: coderws.CompressionDisabled},
		{name: "shared/permessage_deflate", mode: OpenAIWSIngressModeShared, compression: coderws.CompressionContextTakeover, compressed: true},
		{name: "dedicated/identity", mode: OpenAIWSIngressModeDedicated, compression: coderws.CompressionDisabled},
		{name: "dedicated/permessage_deflate", mode: OpenAIWSIngressModeDedicated, compression: coderws.CompressionContextTakeover, compressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func(request []byte, index int) ([][]byte, bool) {
				return wsTrafficCompletedTurn(t, fmt.Sprintf("resp_client_turn_%d", index+1), strings.Repeat("compressible answer ", 40*(index+1))), false
			})
			svc, account := newWSTrafficGatewayService(t, upstream)
			svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			account.Extra = map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
				"openai_apikey_responses_websockets_v2_mode":    tc.mode,
			}
			ingress := startWSClientTrafficIngress(t, svc, account)
			client := dialWSClientTraffic(t, ingress, tc.compression)

			first := upstream.mark()
			turn1 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"`+strings.Repeat("question ", 30)+`"}]}`)
			ingress.waitTurns(t, 1)
			upstreamRequest1, upstreamResponse1 := upstream.since(first)

			// A client keepalive between turns belongs to no turn.
			pingCtx, cancelPing := context.WithTimeout(context.Background(), time.Second)
			pingErr := make(chan error, 1)
			go func() { pingErr <- client.conn.Ping(pingCtx) }()
			second := upstream.mark()
			turn2 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","stream":true,"previous_response_id":"resp_client_turn_1","input":[{"type":"input_text","text":"short follow-up"}]}`)
			cancelPing()
			<-pingErr
			turns := ingress.waitTurns(t, 2)
			upstreamRequest2, upstreamResponse2 := upstream.since(second)

			require.NoError(t, client.conn.Close(coderws.StatusNormalClosure, ""))
			require.NoError(t, ingress.waitDone(t))
			require.Len(t, ingress.waitTurns(t, 2), 2, "the close handshake must not create a turn")
			require.Equal(t, 1, upstream.connCount(), "both turns share the session's upstream socket")
			require.Equal(t, "response.completed", turn1.terminal)
			require.Equal(t, "response.completed", turn2.terminal)
			if tc.compressed {
				require.Less(t, turn1.responseActual, turn1.responseOriginal, "deflated frames must be counted as actual bytes")
			}
			require.NotEqual(t, turn1.responseOriginal, turn2.responseOriginal)

			for i, want := range []struct {
				client                    wsClientTurnBytes
				upstreamReq, upstreamResp int64
			}{{turn1, upstreamRequest1, upstreamResponse1}, {turn2, upstreamRequest2, upstreamResponse2}} {
				usage := turns[i].Usage
				requireWSClientTurnDownstream(t, usage, want.client, tc.compressed)
				require.NotNil(t, usage.Upstream, "turn %d", i+1)
				requireWSTrafficBytes(t, usage.Upstream.Request, int64Ref(want.upstreamReq), int64Ref(want.upstreamReq), false)
				requireWSTrafficBytes(t, usage.Upstream.Response, int64Ref(want.upstreamResp), int64Ref(want.upstreamResp), false)
				require.False(t, turns[i].FinalizedAt.IsZero())
			}
		})
	}
}

func TestOpenAIWSClientTrafficPassthroughMeasuresUpstreamSocketPerTurn(t *testing.T) {
	upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func(request []byte, index int) ([][]byte, bool) {
		return wsTrafficCompletedTurn(t, fmt.Sprintf("resp_passthrough_%d", index+1), strings.Repeat("p", 30*(index+1))), false
	})
	svc, account := newWSTrafficGatewayService(t, upstream)
	svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	account.Extra = map[string]any{
		"openai_apikey_responses_websockets_v2_enabled": true,
		"openai_apikey_responses_websockets_v2_mode":    OpenAIWSIngressModePassthrough,
	}
	ingress := startWSClientTrafficIngress(t, svc, account)
	client := dialWSClientTraffic(t, ingress, coderws.CompressionContextTakeover)

	first := upstream.mark()
	turn1 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","input":"first passthrough turn"}`)
	ingress.waitTurns(t, 1)
	upstreamRequest1, upstreamResponse1 := upstream.since(first)
	second := upstream.mark()
	turn2 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","input":"second"}`)
	turns := ingress.waitTurns(t, 2)
	upstreamRequest2, upstreamResponse2 := upstream.since(second)
	require.NoError(t, client.conn.Close(coderws.StatusNormalClosure, ""))
	_ = ingress.waitDone(t)

	require.Len(t, ingress.waitTurns(t, 2), 2)
	require.Equal(t, 1, upstream.connCount())
	for i, want := range []struct {
		client                    wsClientTurnBytes
		upstreamReq, upstreamResp int64
	}{{turn1, upstreamRequest1, upstreamResponse1}, {turn2, upstreamRequest2, upstreamResponse2}} {
		usage := turns[i].Usage
		requireWSClientTurnDownstream(t, usage, want.client, true)
		require.NotNil(t, usage.Upstream, "turn %d", i+1)
		requireWSTrafficBytes(t, usage.Upstream.Request, int64Ref(want.upstreamReq), int64Ref(want.upstreamReq), false)
		requireWSTrafficBytes(t, usage.Upstream.Response, int64Ref(want.upstreamResp), int64Ref(want.upstreamResp), false)
	}
}

// wsClientTrafficHTTPUpstream sends bridge requests through the instrumented
// upstream transport, as the repository HTTPUpstream does.
type wsClientTrafficHTTPUpstream struct{ client *http.Client }

func (u *wsClientTrafficHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *wsClientTrafficHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.client.Do(req)
}

func TestOpenAIWSClientTrafficHTTPBridgeMeasuresUpstreamTransport(t *testing.T) {
	var mu sync.Mutex
	var requestBytes, responseBytes []int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		index := 0
		mu.Lock()
		index = len(requestBytes)
		requestBytes = append(requestBytes, int64(len(body)))
		mu.Unlock()
		responseID := fmt.Sprintf("resp_bridge_%d", index+1)
		var sse strings.Builder
		for _, event := range wsTrafficCompletedTurn(t, responseID, strings.Repeat("b", 20*(index+1))) {
			sse.WriteString("data: ")
			sse.Write(event)
			sse.WriteString("\n\n")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		n, _ := io.WriteString(w, sse.String())
		mu.Lock()
		responseBytes = append(responseBytes, int64(n))
		mu.Unlock()
	}))
	defer httpServer.Close()
	upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func([]byte, int) ([][]byte, bool) { return nil, false })
	svc, account := newWSTrafficGatewayService(t, upstream)
	svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc.httpUpstream = &wsClientTrafficHTTPUpstream{client: &http.Client{Transport: traffic.NewUpstreamTransport(http.DefaultTransport)}}
	account.Credentials["base_url"] = httpServer.URL
	account.Extra = map[string]any{
		"openai_apikey_responses_websockets_v2_enabled": true,
		"openai_apikey_responses_websockets_v2_mode":    OpenAIWSIngressModeHTTPBridge,
	}
	ingress := startWSClientTrafficIngress(t, svc, account)
	client := dialWSClientTraffic(t, ingress, coderws.CompressionDisabled)

	turn1 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","input":"bridge one"}`)
	ingress.waitTurns(t, 1)
	turn2 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","input":"bridge two, longer"}`)
	turns := ingress.waitTurns(t, 2)
	require.NoError(t, client.conn.Close(coderws.StatusNormalClosure, ""))
	_ = ingress.waitDone(t)

	require.Zero(t, upstream.connCount(), "the bridge never dials the upstream WebSocket")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requestBytes, 2)
	for i, want := range []wsClientTurnBytes{turn1, turn2} {
		usage := turns[i].Usage
		requireWSClientTurnDownstream(t, usage, want, false)
		require.NotNil(t, usage.Upstream, "turn %d", i+1)
		requireWSTrafficBytes(t, usage.Upstream.Request, int64Ref(requestBytes[i]), int64Ref(requestBytes[i]), false)
		requireWSTrafficBytes(t, usage.Upstream.Response, int64Ref(responseBytes[i]), int64Ref(responseBytes[i]), false)
	}
}

func TestOpenAIWSClientTrafficFailedTurnEndsWithSession(t *testing.T) {
	upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func(request []byte, index int) ([][]byte, bool) {
		if index == 0 {
			return wsTrafficCompletedTurn(t, "resp_ok", "fine"), false
		}
		// The second turn is cut off after its first event.
		return wsTrafficCompletedTurn(t, "resp_cut", "lost")[:1], true
	})
	svc, account := newWSTrafficGatewayService(t, upstream)
	ingress := startWSClientTrafficIngress(t, svc, account)
	client := dialWSClientTraffic(t, ingress, coderws.CompressionDisabled)

	client.turn(t, `{"type":"response.create","model":"gpt-5.1","input":"one"}`)
	ingress.waitTurns(t, 1)
	second := upstream.mark()
	turn2 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","input":"two"}`)
	require.Error(t, ingress.waitDone(t))
	turns := ingress.waitTurns(t, 2)
	upstreamRequest, upstreamResponse := upstream.since(second)

	require.Empty(t, turn2.terminal)
	require.Positive(t, turn2.responseOriginal, "the event written before the failure belongs to the failed turn")
	requireWSClientTurnDownstream(t, turns[1].Usage, turn2, false)
	require.NotNil(t, turns[1].Usage.Upstream)
	requireWSTrafficBytes(t, turns[1].Usage.Upstream.Request, int64Ref(upstreamRequest), int64Ref(upstreamRequest), false)
	requireWSTrafficBytes(t, turns[1].Usage.Upstream.Response, int64Ref(upstreamResponse), int64Ref(upstreamResponse), false)
}

func TestOpenAIWSClientTrafficUncountedWriteMakesDeflatedOriginalUnknown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		compression coderws.CompressionMode
		compressed  bool
	}{
		{name: "identity", compression: coderws.CompressionDisabled},
		{name: "permessage_deflate", compression: coderws.CompressionContextTakeover, compressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var turn OpenAIWSClientTurnTraffic
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				meter := NewOpenAIWSClientTraffic()
				meter.SetOnTurnEnd(func(got OpenAIWSClientTurnTraffic) { turn = got })
				ginCtx, _ := gin.CreateTestContext(w)
				conn, err := coderws.Accept(meter.WrapResponseWriter(ginCtx.Writer), r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
				require.NoError(t, err)
				defer func() { _ = conn.CloseNow() }()
				ctx := context.Background()
				_, _, err = meter.ReadClientMessage(ctx, conn, time.Second, coderws.StatusPolicyViolation, "missing")
				require.NoError(t, err)
				meter.BeginTurn()
				message := []byte(strings.Repeat(`{"type":"response.output_text.delta","delta":"zzzz"}`, 20))
				require.NoError(t, meter.WriteClientMessage(ctx, conn, coderws.MessageText, message))
				// A write path that bypasses the meter.
				require.NoError(t, conn.Write(ctx, coderws.MessageText, message))
				meter.Close()
			}))
			defer server.Close()
			httpClient, transport := newOpenAIWSWireCountingClient(nil)
			dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPClient: httpClient, CompressionMode: tc.compression})
			require.NoError(t, err)
			defer func() { _ = conn.CloseNow() }()
			require.NoError(t, conn.Write(dialCtx, coderws.MessageText, []byte(`{"type":"response.create"}`)))
			for i := 0; i < 2; i++ {
				_, _, err = conn.Read(dialCtx)
				require.NoError(t, err)
			}
			<-done
			wire := transport.upgraded()
			require.NotNil(t, turn.Usage)
			response := turn.Usage.Downstream.Response
			require.Equal(t, tc.compressed, response.Compressed)
			require.NotNil(t, response.ActualBytes)
			require.Equal(t, wire.read.payload.Load(), *response.ActualBytes)
			if tc.compressed {
				require.Nil(t, response.OriginalBytes, "an uncounted message cannot be part of a known original")
			} else {
				require.Equal(t, *response.ActualBytes, *response.OriginalBytes)
			}
		})
	}
}

func TestOpenAIWSClientTurnRecordFailureRules(t *testing.T) {
	start := openAIWSClientTrafficMark{wireKnown: true}
	start.MessageRead, start.WireRead, start.MessageWritten, start.WireWritten = 10, 8, 100, 40
	end := start
	end.MessageRead, end.WireRead, end.MessageWritten, end.WireWritten = 30, 20, 400, 90
	end.wireMessagesWritten, end.messagesWritten = 3, 3

	record := func(end openAIWSClientTrafficMark, compressed bool) *traffic.Usage {
		turn := &openAIWSClientTurn{collector: traffic.NewCollector(), start: start}
		turn.record(end, compressed)
		turn.collector.Finalize()
		return turn.collector.Snapshot()
	}

	usage := record(end, true)
	requireWSTrafficBytes(t, usage.Downstream.Request, int64Ref(20), int64Ref(12), true)
	requireWSTrafficBytes(t, usage.Downstream.Response, int64Ref(300), int64Ref(50), true)

	failedWrite := end
	failedWrite.WriteFailures = 1
	usage = record(failedWrite, true)
	requireWSTrafficBytes(t, usage.Downstream.Response, nil, nil, true)
	usage = record(failedWrite, false)
	requireWSTrafficBytes(t, usage.Downstream.Response, nil, nil, false)

	failedRead := end
	failedRead.readFailures = 1
	usage = record(failedRead, true)
	requireWSTrafficBytes(t, usage.Downstream.Request, nil, int64Ref(12), true)
	usage = record(failedRead, false)
	// Without deflate the consumed prefix is both representations.
	requireWSTrafficBytes(t, usage.Downstream.Request, int64Ref(12), int64Ref(12), false)
}

func TestOpenAIWSClientTrafficBeginTurnAfterCloseIsUnattributed(t *testing.T) {
	meter := NewOpenAIWSClientTraffic()
	var ended []OpenAIWSClientTurnTraffic
	meter.SetOnTurnEnd(func(turn OpenAIWSClientTurnTraffic) { ended = append(ended, turn) })
	first := meter.BeginTurn()
	require.NotNil(t, first)
	meter.SetTurnRequestID("resp_1")
	meter.SetAccountID(7)
	// A new request ends the open turn first.
	second := meter.BeginTurn()
	require.NotSame(t, first, second)
	require.True(t, first.Finalized())
	meter.Close()
	require.True(t, second.Finalized())
	require.Nil(t, meter.BeginTurn())
	require.Len(t, ended, 2)
	require.Equal(t, "resp_1", ended[0].RequestID)
	require.Equal(t, int64(7), ended[0].AccountID)
	require.Empty(t, ended[1].RequestID)

	var nilMeter *OpenAIWSClientTraffic
	require.Nil(t, nilMeter.BeginTurn())
	ctx := context.Background()
	require.Equal(t, ctx, nilMeter.TurnContext(ctx))
	nilMeter.EndTurn()
	nilMeter.Close()
}

func TestOpenAIWSFrameMeterCountsDataMessages(t *testing.T) {
	var meter openAIWSFrameMeter
	// Text frame without FIN, its continuation, a ping and a binary frame.
	meter.observe([]byte{0x01, 0x02, 'h', 'i'})
	meter.observe([]byte{0x80, 0x01, '!'})
	meter.observe([]byte{0x89, 0x00})
	meter.observe([]byte{0x82, 0x01, 0x00})
	require.Equal(t, int64(2), meter.messages.Load())
	require.Equal(t, int64(4), meter.payload.Load())
}
