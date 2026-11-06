package reqcompress

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func sharedTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return server, client
}

func sharedTestRegistry(client *redis.Client) *Registry {
	registry := NewRegistry()
	registry.SetRedis(client)
	return registry
}

func requireSharedAlgorithm(t *testing.T, server *miniredis.Miniredis, accountID int64, want Algorithm) {
	t.Helper()
	got, err := server.Get(sharedAlgorithmKey(accountID))
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}

func TestRegistrySharesSettledAlgorithmAcrossInstances(t *testing.T) {
	server, client := sharedTestRedis(t)
	first := sharedTestRegistry(client)
	second := sharedTestRegistry(client)
	const accountID int64 = 701
	first.Sync(accountID, true, time.Time{})
	second.Sync(accountID, true, time.Time{})

	_, err := first.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
	require.Equal(t, 6*time.Hour, sharedAlgorithmTTL)

	// A second process reuses the result without probing br/zstd/deflate again.
	require.Equal(t, AlgorithmBrotli, second.SettledAlgorithm(context.Background(), accountID))
	var sent []string
	_, err = second.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		sent = append(sent, req.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"br"}, sent)

	// Once initialized, the hot path is memory-only and survives a Redis outage.
	server.Close()
	_, err = second.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "br", req.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
}

func TestRegistrySharedWritesSurviveRequestCancellation(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 709
	registry.Sync(accountID, true, time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := newTestRequest(t, testBody()).WithContext(ctx)
	_, err := registry.Execute(accountID, req, func(attempt *http.Request) (*http.Response, error) {
		if attempt.Header.Get("Content-Encoding") == "zstd" {
			// The upstream succeeded, so cancellation of the client-facing request
			// must not discard the negotiated shared result.
			cancel()
			return okResponse("ok"), nil
		}
		return rejectionResponse("unsupported content encoding"), nil
	})
	require.NoError(t, err)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)

	registry.deleteSharedAlgorithm(ctx, accountID, AlgorithmZstd)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

func TestRegistrySharedLoadFailureDoesNotRestartNegotiation(t *testing.T) {
	server, client := sharedTestRedis(t)
	writer := sharedTestRegistry(client)
	observer := sharedTestRegistry(client)
	const accountID int64 = 710
	writer.Sync(accountID, true, time.Time{})
	observer.Sync(accountID, true, time.Time{})

	_, err := writer.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		if attempt.Header.Get("Content-Encoding") == "br" {
			return rejectionResponse("unsupported content encoding"), nil
		}
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)

	// A fresh instance that cannot read the shared result must forward identity
	// and retry initialization later, not probe br and overwrite zstd.
	server.Close()
	var sent []string
	_, err = observer.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		sent = append(sent, attempt.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{""}, sent)
}

func TestRegistrySettledAlgorithmTracksSharedChanges(t *testing.T) {
	server, client := sharedTestRedis(t)
	writer := sharedTestRegistry(client)
	observer := sharedTestRegistry(client)
	const accountID int64 = 708
	writer.Sync(accountID, true, time.Time{})
	observer.Sync(accountID, true, time.Time{})

	_, err := writer.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.Equal(t, AlgorithmBrotli, observer.SettledAlgorithm(context.Background(), accountID))

	// Simulate another instance publishing a replacement and then clearing it.
	// Administrative display reads must not keep returning the observer's first
	// process-local snapshot after the shared value has moved on.
	require.NoError(t, server.Set(sharedAlgorithmKey(accountID), string(AlgorithmZstd)))
	require.Equal(t, AlgorithmZstd, observer.SettledAlgorithm(context.Background(), accountID))
	require.NoError(t, client.Del(context.Background(), sharedAlgorithmKey(accountID)).Err())
	require.Equal(t, AlgorithmNone, observer.SettledAlgorithm(context.Background(), accountID))
}

func TestRegistryRetiresSharedAlgorithmAndPublishesReplacement(t *testing.T) {
	server, client := sharedTestRedis(t)
	first := sharedTestRegistry(client)
	second := sharedTestRegistry(client)
	const accountID int64 = 702
	first.Sync(accountID, true, time.Time{})
	second.Sync(accountID, true, time.Time{})

	_, err := first.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.Equal(t, AlgorithmBrotli, second.SettledAlgorithm(context.Background(), accountID))

	var sent []string
	_, err = second.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		encoding := req.Header.Get("Content-Encoding")
		sent = append(sent, encoding)
		if encoding == "br" {
			return rejectionResponse("unsupported content encoding"), nil
		}
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"br", "zstd"}, sent)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)

	third := sharedTestRegistry(client)
	third.Sync(accountID, true, time.Time{})
	require.Equal(t, AlgorithmZstd, third.SettledAlgorithm(context.Background(), accountID))
}

func TestRegistryResetClearsLocalAndSharedAlgorithm(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 703
	registry.Sync(accountID, true, time.Time{})

	_, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.True(t, server.Exists(sharedAlgorithmKey(accountID)))

	registry.Reset(context.Background(), accountID)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
	require.Equal(t, AlgorithmNone, registry.SettledAlgorithm(context.Background(), accountID))

	// Re-enabling starts with an empty shared snapshot rather than resurrecting
	// the algorithm that was pinned before the administrative reset.
	registry.Sync(accountID, true, time.Time{})
	require.Equal(t, AlgorithmNone, registry.SettledAlgorithm(context.Background(), accountID))
}

func TestRegistryDisableDeletesSharedAlgorithm(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 706
	registry.Sync(accountID, true, time.Time{})

	_, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.True(t, server.Exists(sharedAlgorithmKey(accountID)))

	registry.Sync(accountID, false, time.Time{})
	require.False(t, registry.Enabled(accountID))
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))

	registry.Sync(accountID, true, time.Time{})
	require.Equal(t, AlgorithmNone, registry.SettledAlgorithm(context.Background(), accountID))
}

func TestRegistryDoesNotPersistExplicitlyRejectedIdentity(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 704
	registry.Sync(accountID, true, time.Time{})

	var sent []string
	_, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		encoding := req.Header.Get("Content-Encoding")
		sent = append(sent, encoding)
		if encoding == "" {
			return okResponse("identity"), nil
		}
		return rejectionResponse("unsupported content encoding"), nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"br", "zstd", "deflate", ""}, sent)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

func TestPersistSharedIdentityScriptPreservesCodingAndDeadline(t *testing.T) {
	server, client := sharedTestRedis(t)
	const accountID int64 = 712
	key := sharedAlgorithmKey(accountID)
	run := func() {
		t.Helper()
		require.NoError(t, persistSharedIdentityScript.Run(context.Background(), client,
			[]string{key, sharedInvalidationKey(accountID)}, ambiguousIdentityRetryTTL.Milliseconds()).Err())
	}

	run()
	requireSharedAlgorithm(t, server, accountID, Algorithm(sharedIdentityValue))
	require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(key))

	// Republishing the same identity does not turn its fixed TTL into a sliding
	// expiration, even when the caller supplies a fresh 30-minute PX value.
	server.FastForward(10 * time.Minute)
	run()
	require.Equal(t, 20*time.Minute, server.TTL(key))

	// A coding negotiated by another instance always wins over late identity.
	require.NoError(t, server.Set(key, string(AlgorithmBrotli)))
	server.SetTTL(key, sharedAlgorithmTTL)
	run()
	requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
}

func TestRegistrySharesAmbiguousIdentityAcrossInstancesUntilTTL(t *testing.T) {
	server, client := sharedTestRedis(t)
	first := sharedTestRegistry(client)
	second := sharedTestRegistry(client)
	now := time.Date(2026, 11, 6, 22, 0, 0, 0, time.UTC)
	first.nowFunc = func() time.Time { return now }
	second.nowFunc = func() time.Time { return now }
	const accountID int64 = 711
	first.Sync(accountID, true, time.Time{})
	second.Sync(accountID, true, time.Time{})

	var firstAttempts []string
	resp, err := first.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		encoding := req.Header.Get("Content-Encoding")
		firstAttempts = append(firstAttempts, encoding)
		if encoding == "" {
			return okResponse("identity"), nil
		}
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"detail":"Bad Request"}`))}, nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br", "zstd", "deflate", ""}, firstAttempts)
	requireSharedAlgorithm(t, server, accountID, Algorithm(sharedIdentityValue))
	require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(sharedAlgorithmKey(accountID)))
	require.Equal(t, AlgorithmNone, second.SettledAlgorithm(context.Background(), accountID))

	// The second instance first reads the shared identity at minute 29. It must
	// inherit only the remaining minute, not start a fresh 30-minute timer.
	now = now.Add(29 * time.Minute)
	server.FastForward(29 * time.Minute)
	var secondAttempts []string
	resp, err = second.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		secondAttempts = append(secondAttempts, req.Header.Get("Content-Encoding"))
		require.Empty(t, req.Header.Get("Content-Encoding"))
		return okResponse("identity"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{""}, secondAttempts)
	require.Equal(t, time.Minute, server.TTL(sharedAlgorithmKey(accountID)), "identity reads never renew its TTL")

	// At the shared expiry the second instance reopens candidates. Its success
	// replaces identity with the ordinary six-hour coding result.
	now = now.Add(time.Minute)
	server.FastForward(time.Minute)
	secondAttempts = nil
	resp, err = second.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		secondAttempts = append(secondAttempts, req.Header.Get("Content-Encoding"))
		require.Equal(t, "br", req.Header.Get("Content-Encoding"))
		return okResponse("brotli-recovered"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br"}, secondAttempts)
	requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
}

func TestRegistryDoesNotPersistAlgorithmAfterReset(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 707
	registry.Sync(accountID, true, time.Time{})
	registry.Reset(context.Background(), accountID)

	// Simulate a coded request that completed after the administrative reset.
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

func TestRegistryDeleteSharedAlgorithmDoesNotRemoveDifferentCoding(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 705
	require.NoError(t, server.Set(sharedAlgorithmKey(accountID), "zstd"))

	registry.deleteSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)

	registry.deleteSharedAlgorithm(context.Background(), accountID, AlgorithmZstd)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

// settleTestState enables accountID and settles its local state on algorithm
// without Execute, so refresh behavior is tested independently of the request path.
func settleTestState(t *testing.T, registry *Registry, accountID int64, algorithm Algorithm) *state {
	t.Helper()
	registry.Sync(accountID, true, time.Time{})
	negotiation := registry.existingSlot(accountID).currentState()
	require.NotNil(t, negotiation)
	negotiation.negotiationSucceeded(algorithm)
	return negotiation
}

// docs/REQUEST_COMPRESSION.md: six-hour sliding TTL, refreshed only by requests.
func TestRegistryRefreshSharedAlgorithmSlidesTTLAcrossRequests(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 720
	negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	key := sharedAlgorithmKey(accountID)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))

	for i := 0; i < 3; i++ {
		server.FastForward(5 * time.Hour)
		require.True(t, server.Exists(key), "request %d", i)
		registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
		require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
		requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
	}
	// Without further requests the value finally expires after the full period.
	server.FastForward(sharedAlgorithmTTL)
	require.False(t, server.Exists(key))
}

func TestRegistryRefreshSharedAlgorithmMigratesLegacyKeyWithoutTTL(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 721
	key := sharedAlgorithmKey(accountID)
	require.NoError(t, server.Set(key, "zstd"))
	require.Zero(t, server.TTL(key))

	registry.Sync(accountID, true, time.Time{})
	negotiation := registry.existingSlot(accountID).currentState()
	require.NoError(t, registry.hydrateShared(context.Background(), accountID, negotiation))
	require.Equal(t, AlgorithmZstd, negotiation.settledAlgorithm())

	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
}

func TestRegistryRefreshSharedAlgorithmSurvivesCanceledContext(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 722
	negotiation := settleTestState(t, registry, accountID, AlgorithmDeflate)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmDeflate)
	key := sharedAlgorithmKey(accountID)
	server.SetTTL(key, time.Minute)

	// A failed or canceled client request must still keep the probed coding.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry.refreshSharedAlgorithm(ctx, accountID, negotiation)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
	require.Equal(t, AlgorithmDeflate, negotiation.settledAlgorithm())
}

func TestRegistryRefreshSharedAlgorithmRestoresMissingKey(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 723
	negotiation := settleTestState(t, registry, accountID, AlgorithmZstd)
	key := sharedAlgorithmKey(accountID)
	require.False(t, server.Exists(key), "TTL expiry or Redis restart")

	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
	require.Equal(t, AlgorithmZstd, registry.SettledAlgorithm(context.Background(), accountID))
}

func TestRegistryRefreshSharedAlgorithmDoesNotOverwriteOrExtendDifferentCoding(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 724
	negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
	key := sharedAlgorithmKey(accountID)
	require.NoError(t, server.Set(key, "zstd"))
	server.SetTTL(key, time.Hour)

	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, time.Hour, server.TTL(key))
	require.Equal(t, AlgorithmBrotli, negotiation.settledAlgorithm(), "local state is not changed by refresh")
}

func TestRegistryRefreshSharedAlgorithmDoesNotResurrectAfterResetOrDisable(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 725
	key := sharedAlgorithmKey(accountID)

	stale := settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	registry.Reset(context.Background(), accountID)
	require.False(t, server.Exists(key))
	registry.refreshSharedAlgorithm(context.Background(), accountID, stale)
	require.False(t, server.Exists(key), "reset without a slot")

	// A fresh state for the same account is a different negotiation: the old
	// one must not republish its coding even when the slot exists again.
	registry.Sync(accountID, true, time.Time{})
	registry.refreshSharedAlgorithm(context.Background(), accountID, stale)
	require.False(t, server.Exists(key), "reset then re-enable")

	disabled := settleTestState(t, registry, accountID, AlgorithmZstd)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmZstd)
	registry.Sync(accountID, false, time.Time{})
	require.False(t, server.Exists(key))
	registry.refreshSharedAlgorithm(context.Background(), accountID, disabled)
	require.False(t, server.Exists(key), "disabled")
	registry.Sync(accountID, true, time.Time{})
	registry.refreshSharedAlgorithm(context.Background(), accountID, disabled)
	require.False(t, server.Exists(key), "disabled then re-enabled")
}

func TestRegistryRefreshSharedAlgorithmIgnoresUnsettledState(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 726
	registry.Sync(accountID, true, time.Time{})
	negotiation := registry.existingSlot(accountID).currentState()
	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

func TestRegistrySettledAlgorithmReadDoesNotRenewTTL(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 727
	settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	key := sharedAlgorithmKey(accountID)
	server.SetTTL(key, time.Hour)

	require.Equal(t, AlgorithmBrotli, registry.SettledAlgorithm(context.Background(), accountID))
	require.Equal(t, time.Hour, server.TTL(key), "administrative reads must not keep a coding alive")
}

func TestRegistryRefreshSharedAlgorithmFailureKeepsLocalNegotiation(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 728
	negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
	server.Close()

	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	require.Equal(t, AlgorithmBrotli, negotiation.settledAlgorithm())
	require.Same(t, negotiation, registry.existingSlot(accountID).currentState())
}

func TestRegistryRefreshSharedAlgorithmWithoutRedisIsNoop(t *testing.T) {
	registry := NewRegistry()
	const accountID int64 = 729
	negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
	require.NotPanics(t, func() {
		registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
		registry.refreshSharedAlgorithm(nil, accountID, nil) //nolint:staticcheck // nil context is tolerated
		(*Registry)(nil).refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	})
	require.Equal(t, AlgorithmBrotli, negotiation.settledAlgorithm())
}

func TestClaimSharedRefreshLogRateLimitsPerAccount(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const accountID int64 = 730_000_001
	// The limiter is process-global; make repeated runs independent.
	sharedRefreshLogTimes.Delete(accountID)
	sharedRefreshLogTimes.Delete(accountID + 1)
	require.True(t, claimSharedRefreshLog(accountID, now))
	require.False(t, claimSharedRefreshLog(accountID, now.Add(sharedRefreshLogInterval-time.Second)))
	require.True(t, claimSharedRefreshLog(accountID+1, now))
	require.True(t, claimSharedRefreshLog(accountID, now.Add(sharedRefreshLogInterval)))
}

// delayProxy forwards TCP traffic to miniredis and can hold replies back while
// still delivering commands, which models a slow or stalled Redis that go-redis
// can only escape through its socket deadlines.
type delayProxy struct {
	listener net.Listener
	target   string
	blocked  atomic.Bool
	upstream atomic.Int64 // bytes received from clients
	mu       sync.Mutex
	conns    []net.Conn
}

func newDelayProxy(t *testing.T, target string) *delayProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	proxy := &delayProxy{listener: listener, target: target}
	go proxy.serve()
	t.Cleanup(func() {
		proxy.blocked.Store(false)
		_ = listener.Close()
		proxy.mu.Lock()
		for _, conn := range proxy.conns {
			_ = conn.Close()
		}
		proxy.mu.Unlock()
	})
	return proxy
}

func (p *delayProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := client.Read(buf)
				if n > 0 {
					p.upstream.Add(int64(n))
					if _, werr := server.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					_ = server.Close()
					return
				}
			}
		}()
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := server.Read(buf)
				if n > 0 {
					for p.blocked.Load() {
						time.Sleep(2 * time.Millisecond)
					}
					if _, werr := client.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					if err != io.EOF {
						_ = client.Close()
					}
					return
				}
			}
		}()
	}
}

// waitForUpstream waits until a command sent after the call reaches Redis.
func (p *delayProxy) waitForUpstream(t *testing.T, since int64) {
	t.Helper()
	require.Eventually(t, func() bool { return p.upstream.Load() > since }, 2*time.Second, time.Millisecond)
}

func delayedSharedTestRedis(t *testing.T) (*miniredis.Miniredis, *delayProxy, *redis.Client) {
	t.Helper()
	server := miniredis.RunT(t)
	proxy := newDelayProxy(t, server.Addr())
	client := redis.NewClient(&redis.Options{Addr: proxy.listener.Addr().String(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return server, proxy, client
}

func elapsed(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

func TestRegistryRefreshSharedAlgorithmDoesNotQueueConcurrentRequests(t *testing.T) {
	server, proxy, client := delayedSharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID, otherID int64 = 740, 741
	negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	key := sharedAlgorithmKey(accountID)
	server.SetTTL(key, time.Hour)

	proxy.blocked.Store(true)
	since := proxy.upstream.Load()
	first := make(chan time.Duration, 1)
	go func() {
		first <- elapsed(func() { registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation) })
	}()
	proxy.waitForUpstream(t, since)

	// While the first renewal waits for Redis, same-account requests neither
	// queue behind it nor are blocked on the slot or state locks.
	var wg sync.WaitGroup
	slowest := atomic.Int64{}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := elapsed(func() {
				registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
				require.True(t, registry.Enabled(accountID))
				require.Same(t, negotiation, registry.existingSlot(accountID).currentState())
				_, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
					require.Equal(t, "br", req.Header.Get("Content-Encoding"))
					return okResponse("ok"), nil
				})
				require.NoError(t, err)
			})
			for {
				old := slowest.Load()
				if int64(d) <= old || slowest.CompareAndSwap(old, int64(d)) {
					return
				}
			}
		}()
	}
	wg.Wait()
	require.Less(t, time.Duration(slowest.Load()), sharedRefreshTimeout/2)
	select {
	case <-first:
		t.Fatal("first renewal should still be waiting for Redis")
	default:
	}

	// The stalled renewal is bounded by the short refresh timeout and fails open.
	d := <-first
	require.GreaterOrEqual(t, d, sharedRefreshTimeout/2)
	require.Less(t, d, 5*sharedRefreshTimeout)
	require.Equal(t, AlgorithmBrotli, negotiation.settledAlgorithm())
	require.Same(t, negotiation, registry.existingSlot(accountID).currentState())

	// Non-concurrent requests still renew every time once Redis recovers, and
	// another account never shared the stalled lock.
	proxy.blocked.Store(false)
	other := settleTestState(t, registry, otherID, AlgorithmZstd)
	server.SetTTL(key, time.Hour)
	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
	registry.refreshSharedAlgorithm(context.Background(), otherID, other)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(otherID)))
}

func TestRegistryResetAndDisableOrderAfterInFlightRefresh(t *testing.T) {
	for name, clear := range map[string]func(*Registry, int64){
		"reset":   func(r *Registry, id int64) { r.Reset(context.Background(), id) },
		"disable": func(r *Registry, id int64) { r.Sync(id, false, time.Time{}) },
	} {
		t.Run(name, func(t *testing.T) {
			server, proxy, client := delayedSharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const accountID, otherID int64 = 742, 743
			negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
			registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
			key := sharedAlgorithmKey(accountID)
			server.Del(key)

			proxy.blocked.Store(true)
			since := proxy.upstream.Load()
			refreshed := make(chan struct{})
			go func() {
				defer close(refreshed)
				registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
			}()
			proxy.waitForUpstream(t, since)

			cleared := make(chan struct{})
			go func() {
				defer close(cleared)
				clear(registry, accountID)
			}()
			// The local transition is immediate; only the Redis delete waits.
			require.Eventually(t, func() bool { return !registry.Enabled(accountID) }, time.Second, time.Millisecond)
			select {
			case <-cleared:
				t.Fatal("delete must wait for the in-flight renewal")
			default:
			}

			// Other accounts, including creating a new slot, are not blocked by the
			// pending delete (the stalled Redis is not touched here).
			require.Less(t, elapsed(func() {
				registry.Sync(otherID, true, time.Time{})
				require.True(t, registry.Enabled(otherID))
				require.NotNil(t, registry.existingSlot(otherID).currentState())
				registry.Sync(otherID, true, time.Time{})
			}), sharedRefreshTimeout/2)

			// Let the renewal finish before its timeout: it restores the key, and
			// the ordered delete then removes it.
			time.Sleep(50 * time.Millisecond)
			proxy.blocked.Store(false)
			<-refreshed
			select {
			case <-cleared:
			case <-time.After(5 * time.Second):
				t.Fatal("delete did not finish")
			}
			require.False(t, server.Exists(key))

			// The replaced state cannot revive the coding afterwards.
			registry.Sync(accountID, true, time.Time{})
			registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
			require.False(t, server.Exists(key))
		})
	}
}

func TestRegistryResetAfterConcurrentRefreshesLeavesNoKey(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 744
	negotiation := settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	registry.Reset(context.Background(), accountID)
	close(stop)
	wg.Wait()
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

func requireInvalidationMarker(t *testing.T, server *miniredis.Miniredis, accountID int64, want bool) {
	t.Helper()
	require.Equal(t, want, server.Exists(sharedInvalidationKey(accountID)))
}

func TestRegistryStaleInstanceCannotRestoreAfterResetOrRetire(t *testing.T) {
	for name, invalidate := range map[string]func(*Registry, int64){
		"reset":   func(r *Registry, id int64) { r.Reset(context.Background(), id) },
		"disable": func(r *Registry, id int64) { r.Sync(id, false, time.Time{}) },
		"retire":  func(r *Registry, id int64) { r.deleteSharedAlgorithm(context.Background(), id, AlgorithmBrotli) },
	} {
		t.Run(name, func(t *testing.T) {
			server, client := sharedTestRedis(t)
			a, b := sharedTestRegistry(client), sharedTestRegistry(client)
			const accountID int64 = 750
			settleTestState(t, a, accountID, AlgorithmBrotli)
			stale := settleTestState(t, b, accountID, AlgorithmBrotli)
			a.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
			b.refreshSharedAlgorithm(context.Background(), accountID, stale)
			requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)

			invalidate(a, accountID)
			require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
			requireInvalidationMarker(t, server, accountID, true)

			// B's local coding is still valid for its own requests but must not
			// republish the invalidated value.
			b.refreshSharedAlgorithm(context.Background(), accountID, stale)
			require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
			require.Equal(t, AlgorithmBrotli, stale.settledAlgorithm())
			require.Same(t, stale, b.existingSlot(accountID).currentState())
		})
	}
}

func TestRegistryLateRefreshScriptCannotReviveAfterReset(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 751
	settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	registry.Reset(context.Background(), accountID)

	// A renewal whose command reaches Redis only after the reset.
	result, err := refreshSharedAlgorithmScript.Run(context.Background(), client,
		[]string{sharedAlgorithmKey(accountID), sharedInvalidationKey(accountID)},
		string(AlgorithmBrotli), sharedAlgorithmTTL.Milliseconds()).Int()
	require.NoError(t, err)
	require.Equal(t, 3, result)
	require.False(t, server.Exists(sharedAlgorithmKey(accountID)))
}

func TestRegistryRealNegotiationClearsInvalidationMarker(t *testing.T) {
	server, client := sharedTestRedis(t)
	a, b := sharedTestRegistry(client), sharedTestRegistry(client)
	const accountID int64 = 752
	settleTestState(t, a, accountID, AlgorithmBrotli)
	stale := settleTestState(t, b, accountID, AlgorithmBrotli)
	a.Reset(context.Background(), accountID)
	requireInvalidationMarker(t, server, accountID, true)

	// The administrator re-enables the account and a probe succeeds again.
	fresh := settleTestState(t, a, accountID, AlgorithmBrotli)
	a.persistSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
	requireInvalidationMarker(t, server, accountID, false)

	// With the marker gone both instances renew the real value as usual.
	server.SetTTL(sharedAlgorithmKey(accountID), time.Hour)
	b.refreshSharedAlgorithm(context.Background(), accountID, stale)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
	a.refreshSharedAlgorithm(context.Background(), accountID, fresh)
	requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)

	// A different real negotiation replaces the coding; stale B does not undo it.
	settleTestState(t, a, accountID, AlgorithmZstd)
	a.persistSharedAlgorithm(context.Background(), accountID, AlgorithmZstd)
	server.SetTTL(sharedAlgorithmKey(accountID), time.Hour)
	b.refreshSharedAlgorithm(context.Background(), accountID, stale)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, time.Hour, server.TTL(sharedAlgorithmKey(accountID)))
}

func TestRegistryInvalidationMarkerIsBounded(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 753
	settleTestState(t, registry, accountID, AlgorithmBrotli)
	registry.Reset(context.Background(), accountID)
	require.Equal(t, sharedInvalidationTTL, server.TTL(sharedInvalidationKey(accountID)))
	require.Equal(t, 6*time.Hour, sharedInvalidationTTL)

	// Repeated resets rewrite the marker with the same bound instead of growing it.
	server.FastForward(time.Hour)
	registry.Reset(context.Background(), accountID)
	require.Equal(t, sharedInvalidationTTL, server.TTL(sharedInvalidationKey(accountID)))

	// After the bound the marker disappears and no key is left behind.
	server.FastForward(sharedInvalidationTTL + time.Second)
	require.Empty(t, server.Keys())
}

func TestRegistryRetireOfDifferentCodingKeepsCodingAndMarker(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 754

	// A stale retirement of br while another instance published zstd: no marker.
	require.NoError(t, server.Set(sharedAlgorithmKey(accountID), "zstd"))
	registry.deleteSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	requireInvalidationMarker(t, server, accountID, false)

	// With a marker already present, a mismatching retirement leaves both alone.
	require.NoError(t, server.Set(sharedInvalidationKey(accountID), "1"))
	server.SetTTL(sharedInvalidationKey(accountID), time.Hour)
	server.SetTTL(sharedAlgorithmKey(accountID), 2*time.Hour)
	registry.deleteSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, time.Hour, server.TTL(sharedInvalidationKey(accountID)))
	require.Equal(t, 2*time.Hour, server.TTL(sharedAlgorithmKey(accountID)))
}

func TestRegistryRetireWithAbsentKeyWritesMarker(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 755
	registry.deleteSharedAlgorithm(context.Background(), accountID, AlgorithmBrotli)
	requireInvalidationMarker(t, server, accountID, true)
}

func TestRegistryRefreshRestoresAfterTTLExpiryOrFlushWithoutMarker(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 756
	negotiation := settleTestState(t, registry, accountID, AlgorithmZstd)
	registry.persistSharedAlgorithm(context.Background(), accountID, AlgorithmZstd)
	key := sharedAlgorithmKey(accountID)

	server.FastForward(sharedAlgorithmTTL + time.Second)
	require.False(t, server.Exists(key))
	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))

	server.FlushAll()
	registry.refreshSharedAlgorithm(context.Background(), accountID, negotiation)
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	requireInvalidationMarker(t, server, accountID, false)
}
