package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// docs/REQUEST_COMPRESSION.md: through the real transport, a settled coding
// the upstream starts rejecting with the ChatGPT Codex detail-only 400 is
// replayed once as identity. The caller only sees the plain success, the
// entity arrives intact both times, the usage record describes the delivered
// plain request, and the account stays on identity for the shared 30 minutes.
func TestUpstreamRequestCompressionReplaysCodingSensitiveFailureAsIdentity(t *testing.T) {
	const accountID int64 = 990000005
	cache := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: cache.Addr(), MaxRetries: -1})
	reqcompress.ConfigureRedis(redisClient)
	reqcompress.SyncAccount(accountID, true, time.Time{})
	t.Cleanup(func() {
		reqcompress.ResetAccount(context.Background(), accountID)
		reqcompress.ConfigureRedis(nil)
		require.NoError(t, redisClient.Close())
	})

	original := bytes.Repeat([]byte(`{"type":"message","role":"user","content":"replay me intact"},`), 100)
	var rejectCoded atomic.Bool
	var mu sync.Mutex
	var attempts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		coding := r.Header.Get("Content-Encoding")
		mu.Lock()
		attempts = append(attempts, coding)
		mu.Unlock()
		decoded, err := pkghttputil.ReadRequestBodyWithPrealloc(r)
		if err != nil || !bytes.Equal(original, decoded) {
			t.Errorf("receiver got %d bytes (err %v), want %d intact bytes", len(decoded), err, len(original))
		}
		if coding != "" && rejectCoded.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"detail":"Bad Request"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	send := func() (int, string, *traffic.Usage) {
		t.Helper()
		collector := traffic.NewCollector()
		ctx := traffic.WithCollector(context.Background(), collector)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/responses", bytes.NewReader(original))
		require.NoError(t, err)
		resp, err := doUpstreamRequestWithCompression(server.Client(), req, accountID)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		collector.Finalize()
		return resp.StatusCode, string(body), collector.Snapshot()
	}
	takeAttempts := func() []string {
		mu.Lock()
		defer mu.Unlock()
		taken := attempts
		attempts = nil
		return taken
	}

	status, _, _ := send()
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"br"}, takeAttempts())

	rejectCoded.Store(true)
	status, body, usage := send()
	require.Equal(t, http.StatusOK, status, "the caller never sees the coded failure")
	require.JSONEq(t, `{"ok":true}`, body)
	require.Equal(t, []string{"br", ""}, takeAttempts())
	require.NotNil(t, usage.Upstream.Request)
	require.False(t, usage.Upstream.Request.Compressed, "usage describes the delivered plain request")
	require.EqualValues(t, len(original), *usage.Upstream.Request.ActualBytes)
	key := "reqcompress:account:990000005:algorithm"
	coding, err := cache.Get(key)
	require.NoError(t, err)
	require.Equal(t, "identity", coding)
	require.LessOrEqual(t, cache.TTL(key), 30*time.Minute)
	require.Greater(t, cache.TTL(key), 29*time.Minute)

	status, _, _ = send()
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{""}, takeAttempts(), "identity is pinned instead of failing again")
}
