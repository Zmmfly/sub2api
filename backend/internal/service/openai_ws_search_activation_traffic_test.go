package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Redirect only the OAuth endpoint, retaining the production dialer, sockets,
// wire counters and pool. No external service is contacted by this test.
type wsSearchTrafficDialer struct {
	url   string
	inner openAIWSClientDialer
}

func (d *wsSearchTrafficDialer) Dial(ctx context.Context, _ string, headers http.Header, proxyURL string) (openAIWSClientConn, int, http.Header, error) {
	return d.inner.Dial(ctx, d.url, headers, proxyURL)
}

func TestOpenAIWSIngressWebSearchReconnectTrafficPerTurn(t *testing.T) {
	upstream := newWSTrafficUpstream(t, coderws.CompressionDisabled, func(_ []byte, index int) ([][]byte, bool) {
		return wsTrafficCompletedTurn(t, fmt.Sprintf("resp_search_traffic_%d", index+1), strings.Repeat("distinct answer ", 40*(index+1))), false
	})
	svc, account := newWSTrafficGatewayService(t, upstream)
	account.Type = AccountTypeOAuth
	account.Credentials["access_token"] = "test-token"
	svc.openaiWSPool.setClientDialerForTest(&wsSearchTrafficDialer{
		url:   "ws" + strings.TrimPrefix(upstream.server.URL, "http"),
		inner: newDefaultOpenAIWSClientDialer(),
	})
	ingress := startWSClientTrafficIngress(t, svc, account)
	client := dialWSClientTraffic(t, ingress, coderws.CompressionContextTakeover)

	first := upstream.mark()
	turn1 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","store":false,"input":[{"type":"message","role":"user","content":"original question"}]}`)
	firstTurns := ingress.waitTurns(t, 1)
	require.Equal(t, "response.completed", turn1.terminal)
	// The upstream helper records sent payloads just after Write returns.
	require.Eventually(t, func() bool { return upstream.mark().sent == 3 }, time.Second, time.Millisecond)
	request1, response1 := upstream.since(first)
	require.Equal(t, 1, upstream.connCount())

	second := upstream.mark()
	turn2 := client.turn(t, `{"type":"response.create","model":"gpt-5.1","store":false,"previous_response_id":"resp_search_traffic_1","tools":[{"type":"web_search"}],"input":[{"type":"message","role":"user","content":"now search the web"}]}`)
	turns := ingress.waitTurns(t, 2)
	require.Equal(t, "response.completed", turn2.terminal)
	require.Eventually(t, func() bool { return upstream.mark().sent == 6 }, time.Second, time.Millisecond)
	request2, response2 := upstream.since(second)
	require.Equal(t, 2, upstream.connCount(), "search activation must replace the no-search socket")

	upstream.mu.Lock()
	requests := append([][]byte(nil), upstream.received...)
	upstream.mu.Unlock()
	require.Len(t, requests, 2, "each turn must be sent once, on its own socket")
	require.False(t, openAIWSWebSearchDeclared(requests[0]))
	require.True(t, openAIWSWebSearchDeclared(requests[1]))
	require.False(t, gjson.GetBytes(requests[1], "previous_response_id").Exists())
	require.Contains(t, string(requests[1]), "original question", "the second turn must account for the rebuilt full history")
	require.Contains(t, string(requests[1]), "distinct answer")
	require.Greater(t, request2, request1)
	require.NotEqual(t, response1, response2)

	require.NoError(t, client.conn.Close(coderws.StatusNormalClosure, ""))
	require.NoError(t, ingress.waitDone(t))
	require.Len(t, ingress.waitTurns(t, 2), 2, "socket replacement and close must not add traffic turns")
	for i, want := range []struct {
		client            wsClientTurnBytes
		request, response int64
	}{{turn1, request1, response1}, {turn2, request2, response2}} {
		usage := turns[i].Usage
		requireWSClientTurnDownstream(t, usage, want.client, true)
		require.Less(t, want.client.responseActual, want.client.responseOriginal)
		require.NotNil(t, usage.Upstream)
		// Identity upstream frames have equal original and actual payload bytes.
		// Exact equality excludes lost bytes, doubled prior history, handshakes,
		// and leakage from the replacement socket into the first turn.
		requireWSTrafficBytes(t, usage.Upstream.Request, int64Ref(want.request), int64Ref(want.request), false)
		requireWSTrafficBytes(t, usage.Upstream.Response, int64Ref(want.response), int64Ref(want.response), false)
	}
	// Recheck the snapshot captured before reconnect, not just the final list.
	requireWSTrafficBytes(t, firstTurns[0].Usage.Upstream.Request, int64Ref(request1), int64Ref(request1), false)
	requireWSTrafficBytes(t, firstTurns[0].Usage.Upstream.Response, int64Ref(response1), int64Ref(response1), false)
}
