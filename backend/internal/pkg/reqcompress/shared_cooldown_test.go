package reqcompress

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A stale instance that independently recovers through identity must inherit
// the already published fixed deadline, rather than starting a new local one.
func TestSettledInstancesShareIdentityDeadlineWithoutAnotherFailure(t *testing.T) {
	server, client := sharedTestRedis(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	first, stale := sharedTestRegistry(client), sharedTestRegistry(client)
	first.nowFunc, stale.nowFunc = clock, clock
	const accountID int64 = 987654321
	settleBrotli(t, first, accountID)
	stale.Sync(accountID, true, time.Time{})
	compressedOK := func(attempt *http.Request) (*http.Response, error) {
		require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
		return okResponse("compressed"), nil
	}
	resp, err := stale.Execute(accountID, newTestRequest(t, testBody()), compressedOK)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	recoverWithIdentity := func(attempt *http.Request) (*http.Response, error) {
		if attempt.Header.Get("Content-Encoding") != "" {
			return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
		}
		return okResponse("plain"), nil
	}
	// A publishes identity until T+30m.
	resp, err = first.Execute(accountID, newTestRequest(t, testBody()), recoverWithIdentity)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	sharedDeadline := now.Add(ambiguousIdentityRetryTTL)
	require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(sharedAlgorithmKey(accountID)))
	// B still holds br locally at T+20m, but must observe shared identity
	// before forwarding, without requiring its own upstream failure.
	now = now.Add(20 * time.Minute)
	server.FastForward(20 * time.Minute)
	var staleDuringCooldown []string
	resp, err = stale.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		staleDuringCooldown = append(staleDuringCooldown, attempt.Header.Get("Content-Encoding"))
		return recoverWithIdentity(attempt)
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{""}, staleDuringCooldown, "settled instances honor shared cooldown before another failure")
	require.Equal(t, 10*time.Minute, server.TTL(sharedAlgorithmKey(accountID)), "shared identity was not extended")
	staleDeadline, pinned := stale.existingSlot(accountID).currentState().identityRetry()
	require.True(t, pinned)
	if !staleDeadline.Equal(sharedDeadline) {
		t.Errorf("stale instance local deadline = %s, want existing shared deadline %s", staleDeadline, sharedDeadline)
	}
	// At T+30m A can re-probe br. B must also stop pinning identity then.
	now = now.Add(10 * time.Minute)
	server.FastForward(10 * time.Minute)
	resp, err = first.Execute(accountID, newTestRequest(t, testBody()), compressedOK)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	var staleAttempts []string
	resp, err = stale.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		staleAttempts = append(staleAttempts, attempt.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br"}, staleAttempts, "B should re-probe at the shared deadline, not remain plain until T+50m")
}

func TestInflightReplayUsesExistingSharedDeadlineOrExpiry(t *testing.T) {
	for _, elapsed := range []time.Duration{20 * time.Minute, 31 * time.Minute} {
		t.Run(elapsed.String(), func(t *testing.T) {
			server, client := sharedTestRedis(t)
			first, stale := sharedTestRegistry(client), sharedTestRegistry(client)
			now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
			start := now
			first.nowFunc = func() time.Time { return now }
			stale.nowFunc = func() time.Time { return now }
			const id int64 = 987654322
			settleBrotli(t, first, id)
			stale.Sync(id, true, time.Time{})
			var attempts []string
			resp, err := stale.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
				coding := req.Header.Get("Content-Encoding")
				attempts = append(attempts, coding)
				if coding == "" {
					return okResponse("plain replay"), nil
				}
				// B has already read br and uploaded a coded request. A now
				// publishes identity before B's delayed coded response arrives.
				other, err := first.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Content-Encoding") != "" {
						return codedFailure(400, `{"detail":"Bad Request"}`), nil
					}
					return okResponse("plain"), nil
				})
				require.NoError(t, err)
				require.NoError(t, other.Body.Close())
				now = now.Add(elapsed)
				server.FastForward(elapsed)
				return codedFailure(400, `{"detail":"Bad Request"}`), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []string{"br", ""}, attempts, "one plain replay for the already in-flight coded request")
			deadline, pinned := stale.existingSlot(id).currentState().identityRetry()
			if elapsed < ambiguousIdentityRetryTTL {
				require.True(t, pinned)
				require.Equal(t, start.Add(ambiguousIdentityRetryTTL), deadline)
				require.Equal(t, ambiguousIdentityRetryTTL-elapsed, server.TTL(sharedAlgorithmKey(id)))
			} else {
				require.False(t, pinned)
				require.False(t, server.Exists(sharedAlgorithmKey(id)), "expired identity cannot be renewed by delayed replay")
			}
		})
	}
}
