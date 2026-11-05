package httputil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/andybalholm/brotli"
	"github.com/stretchr/testify/require"
)

func TestBrotliRequestBudgetPreservesTrafficAccounting(t *testing.T) {
	payload := bytes.Repeat([]byte("budgeted request traffic "), 200)
	var compressed bytes.Buffer
	writer := brotli.NewWriter(&compressed)
	_, err := writer.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	for _, limit := range []int64{2048, 1 << 20} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			budget := NewBodyMemoryBudget(limit)
			collector := traffic.NewCollector()
			counter := collector.Begin(traffic.DownstreamRequest, true)
			ctx, release := WithRequestBodyMemoryBudget(traffic.WithCollector(context.Background(), collector), budget)
			defer release()
			req := newRequestWithBody(t, compressed.Bytes(), "br").WithContext(ctx)
			req.Body = traffic.ObserveBody(req.Body, counter, false, req.ContentLength)
			body, err := ReadRequestBodyWithPrealloc(req)
			collector.Finalize()
			usage := collector.Snapshot()
			require.NotNil(t, usage.Downstream.Request.ActualBytes)
			require.Equal(t, int64(compressed.Len()), *usage.Downstream.Request.ActualBytes)
			if limit == 2048 {
				require.True(t, errors.Is(err, ErrRequestBodyMemoryBudget))
				require.Nil(t, body)
				require.Nil(t, usage.Downstream.Request.OriginalBytes)
				require.Zero(t, requestBodyBudgetUsed(budget))
				return
			}
			require.NoError(t, err)
			require.Equal(t, payload, body)
			require.NotNil(t, usage.Downstream.Request.OriginalBytes)
			require.Equal(t, int64(len(payload)), *usage.Downstream.Request.OriginalBytes)
			require.Equal(t, int64(cap(body)), requestBodyBudgetUsed(budget))
			require.Empty(t, req.Header.Get("Content-Encoding"))
			release()
			require.Zero(t, requestBodyBudgetUsed(budget))
		})
	}
}
