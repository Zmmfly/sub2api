package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// A WS attempt that ends before any upstream message (here: invalid state)
// exchanged nothing, so later HTTP attempts are the complete upstream sum.
func TestOpenAIWSAttemptWithoutMessagesKeepsHTTPFallbackUpstream(t *testing.T) {
	for _, collectorInCallContext := range []bool{false, true} {
		collector := traffic.NewCollector()
		requestContext := traffic.WithCollector(context.Background(), collector)
		ctx := context.Background()
		if collectorInCallContext {
			ctx = requestContext
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(requestContext)
		downstream := collector.Begin(traffic.DownstreamResponse, false)
		downstream.AddActual(7)
		downstream.AddOriginal(7)
		downstream.CompleteActual()
		downstream.CompleteOriginal()
		var service *OpenAIGatewayService
		_, err := service.forwardOpenAIWSV2(ctx, c, nil, nil, "", "", "", OpenAIWSProtocolDecision{}, false, false, "", "", time.Now(), 0, "", nil)
		require.Error(t, err)
		for _, direction := range []traffic.Direction{traffic.UpstreamRequest, traffic.UpstreamResponse} {
			body := collector.Begin(direction, false)
			body.AddActual(13)
			body.AddOriginal(13)
			body.CompleteActual()
			body.CompleteOriginal()
		}
		collector.Finalize()
		usage := collector.Snapshot()
		require.NotNil(t, usage.Upstream)
		require.EqualValues(t, 13, *usage.Upstream.Request.ActualBytes)
		require.EqualValues(t, 13, *usage.Upstream.Response.OriginalBytes)
		require.EqualValues(t, 7, *usage.Downstream.Response.ActualBytes)
	}
}
