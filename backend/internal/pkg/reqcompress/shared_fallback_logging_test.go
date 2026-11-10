package reqcompress

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExecuteSharedBusyFallbackDoesNotWarn(t *testing.T) {
	_, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 9824101
	registry.Sync(accountID, true, time.Time{})
	slot := registry.existingSlot(accountID)
	slot.sharedMu.Lock()
	defer slot.sharedMu.Unlock()
	logs := captureLogs(t)
	for range 3 {
		resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
			require.Empty(t, req.Header.Get("Content-Encoding"))
			return okResponse("identity during contention"), nil
		})
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	require.NotContains(t, logs.String(), "level=WARN")
	require.Equal(t, 3, strings.Count(logs.String(), `level=DEBUG msg="reqcompress shared state busy"`))
	_, claimed := sharedRefreshLogTimes.Load(accountID)
	require.False(t, claimed, "normal contention must not consume the error warning budget")
}

func TestExecuteSharedReadFailureWarningsAreRateLimitedPerAccount(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 9824102
	for _, id := range []int64{accountID, accountID + 1} {
		sharedRefreshLogTimes.Delete(id)
		t.Cleanup(func() { sharedRefreshLogTimes.Delete(id) })
		registry.Sync(id, true, time.Time{})
	}
	server.SetError("ERR shared state temporarily unavailable")
	logs := captureLogs(t)
	for _, id := range []int64{accountID, accountID, accountID, accountID + 1} {
		resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
			require.Empty(t, req.Header.Get("Content-Encoding"))
			return okResponse("identity during outage"), nil
		})
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	require.Equal(t, 2, strings.Count(logs.String(), `level=WARN msg="reqcompress shared state load failed"`))
	require.Equal(t, 2, strings.Count(logs.String(), `level=DEBUG msg="reqcompress shared state load failed"`))
}
