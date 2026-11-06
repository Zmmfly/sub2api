package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/reqcompress"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// This real-transport path also exercises the receiver's body decoder: a
// compressed POST must stay replayable, length-correct and intact through
// HTTP failures instead of amplifying bandwidth with identity retries.
func TestUpstreamRequestCompressionPreservesVerifiedCodingAcrossHTTPFailures(t *testing.T) {
	const accountID int64 = 990000004
	cache := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: cache.Addr(), MaxRetries: -1})
	reqcompress.ConfigureRedis(redisClient)
	reqcompress.SyncAccount(accountID, true, time.Time{})
	t.Cleanup(func() {
		reqcompress.ResetAccount(context.Background(), accountID)
		reqcompress.ConfigureRedis(nil)
		require.NoError(t, redisClient.Close())
	})

	var responseStatus atomic.Int32
	responseStatus.Store(http.StatusOK)
	original := bytes.Repeat([]byte("request body must arrive intact "), 100)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if got := r.Header.Get("Content-Encoding"); got != "br" {
			t.Errorf("unexpected coding %q: never replay a verified coding as identity", got)
		}
		if r.ContentLength <= 0 || r.ContentLength >= int64(len(original)) {
			t.Errorf("compressed Content-Length = %d, original = %d", r.ContentLength, len(original))
		}
		decoded, err := pkghttputil.ReadRequestBodyWithPrealloc(r)
		if err != nil {
			t.Errorf("receiver failed to read compressed body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !bytes.Equal(original, decoded) {
			t.Errorf("receiver body differs: got %d bytes, want %d", len(decoded), len(original))
		}
		status := int(responseStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"ok":true}`)
		} else {
			_, _ = io.WriteString(w, `{"error":{"message":"Failed to read request body","type":"invalid_request_error"}}`)
		}
	}))
	defer server.Close()

	for i, status := range []int{200, 400, 403, 415, 500, 502, 503, 504, 200} {
		responseStatus.Store(int32(status))
		cache.FastForward(time.Hour)
		collector := traffic.NewCollector()
		ctx := traffic.WithCollector(context.Background(), collector)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", bytes.NewReader(original))
		require.NoError(t, err)
		resp, err := doUpstreamRequestWithCompression(server.Client(), req, accountID)
		require.NoError(t, err)
		require.Equal(t, status, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		if status != http.StatusOK {
			require.Contains(t, string(body), "Failed to read request body")
		}
		require.EqualValues(t, i+1, attempts.Load(), "one wire request per caller request")
		key := "reqcompress:account:990000004:algorithm"
		coding, err := cache.Get(key)
		require.NoError(t, err)
		require.Equal(t, "br", coding)
		require.Equal(t, 6*time.Hour, cache.TTL(key))
		collector.Finalize()
		usage := collector.Snapshot()
		require.NotNil(t, usage.Upstream.Request)
		require.True(t, usage.Upstream.Request.Compressed)
		require.EqualValues(t, len(original), *usage.Upstream.Request.OriginalBytes)
		require.Less(t, *usage.Upstream.Request.ActualBytes, int64(len(original)))
	}
}
