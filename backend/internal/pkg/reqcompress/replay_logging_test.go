package reqcompress

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplayRecoveryLogReflectsSharedOutcome(t *testing.T) {
	for _, scenario := range []string{"newer_coding", "existing_identity", "write_failure", "reset"} {
		t.Run(scenario, func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			registry.nowFunc = func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) }
			const accountID int64 = 9824990
			sharedRefreshLogTimes.Delete(accountID)
			t.Cleanup(func() { sharedRefreshLogTimes.Delete(accountID) })
			settleBrotli(t, registry, accountID)
			logs := captureLogs(t)
			resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
				if attempt.Header.Get("Content-Encoding") != "" {
					return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
				}
				switch scenario {
				case "newer_coding":
					require.NoError(t, server.Set(sharedAlgorithmKey(accountID), "zstd"))
					server.SetTTL(sharedAlgorithmKey(accountID), sharedAlgorithmTTL)
				case "existing_identity":
					require.NoError(t, server.Set(sharedAlgorithmKey(accountID), sharedIdentityValue))
					server.SetTTL(sharedAlgorithmKey(accountID), 10*time.Minute)
				case "write_failure":
					server.SetError("ERR injected publication failure")
				case "reset":
					registry.Reset(attempt.Context(), accountID)
					registry.Sync(accountID, true, time.Time{})
				}
				return okResponse("plain recovery"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Contains(t, logs.String(), `msg="reqcompress identity replay recovered"`)
			if scenario == "existing_identity" {
				require.Contains(t, logs.String(), "identity_pinned=true")
				require.Contains(t, logs.String(), "identity_ttl_seconds=600")
			} else {
				require.Contains(t, logs.String(), "identity_pinned=false")
				require.Contains(t, logs.String(), "identity_ttl_seconds=0")
			}
			require.NotContains(t, logs.String(), "identity_ttl_seconds=1800")
		})
	}
}

func TestInitialIdentityLogDoesNotClaimFailedSharedPublication(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 9824991
	sharedRefreshLogTimes.Delete(accountID)
	t.Cleanup(func() { sharedRefreshLogTimes.Delete(accountID) })
	registry.Sync(accountID, true, time.Time{})
	logs := captureLogs(t)
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		if attempt.Header.Get("Content-Encoding") != "" {
			return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
		}
		server.SetError("ERR injected publication failure")
		return okResponse("plain recovery"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, logs.String(), "identity_pinned=false")
	require.Contains(t, logs.String(), "identity_ttl_seconds=0")
	require.NotContains(t, logs.String(), `msg="reqcompress pinned identity"`)
}
