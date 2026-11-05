package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/reqcompress"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/stretchr/testify/require"
)

func TestCompressedUpstreamAttemptRetainsDrainBudgetAndTraffic(t *testing.T) {
	const accountID int64 = 990000003
	reqcompress.SyncAccount(accountID, true, time.Time{})
	t.Cleanup(func() { reqcompress.SyncAccount(accountID, false, time.Time{}) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": ping\n\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	collector := traffic.NewCollector()
	client, cancel := context.WithCancel(traffic.WithCollector(context.Background(), collector))
	defer cancel()
	payload := strings.Repeat("compressible request ", 200)
	req, err := http.NewRequestWithContext(pkghttputil.DetachUpstreamContext(client), http.MethodPost, server.URL, strings.NewReader(payload))
	require.NoError(t, err)
	resp, err := doUpstreamRequestWithCompression(server.Client(), req, accountID, 40*time.Millisecond)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		finished <- err
	}()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("compressed attempt lost the configured drain timeout")
	}
	require.NoError(t, resp.Body.Close())
	collector.Finalize()
	usage := collector.Snapshot()
	require.NotNil(t, usage.Upstream.Request)
	require.True(t, usage.Upstream.Request.Compressed)
	require.NotNil(t, usage.Upstream.Request.OriginalBytes)
	require.NotNil(t, usage.Upstream.Request.ActualBytes)
	require.Equal(t, int64(len(payload)), *usage.Upstream.Request.OriginalBytes)
	require.Less(t, *usage.Upstream.Request.ActualBytes, int64(len(payload)))
}
