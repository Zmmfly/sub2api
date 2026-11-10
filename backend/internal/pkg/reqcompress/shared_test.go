package reqcompress

import (
	"context"
	"io"
	"net"
	"net/http"
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

	// Redis remains authoritative: an outage sends only this request plain.
	server.Close()
	_, err = second.Execute(accountID, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Empty(t, req.Header.Get("Content-Encoding"))
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

	snapshot := readRegistrySnapshot(t, registry, accountID)
	registry.deleteSharedAlgorithm(ctx, accountID, AlgorithmZstd, snapshot)
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

// readRegistrySnapshot is a request boundary without an upstream attempt.
func readRegistrySnapshot(t *testing.T, registry *Registry, accountID int64) *sharedSnapshot {
	t.Helper()
	registry.Sync(accountID, true, time.Time{})
	snapshot, err := registry.syncShared(context.Background(), accountID, registry.existingSlot(accountID).currentState())
	require.NoError(t, err)
	return snapshot
}

func publishTestAlgorithm(t *testing.T, registry *Registry, accountID int64, algorithm Algorithm) *state {
	t.Helper()
	snapshot := readRegistrySnapshot(t, registry, accountID)
	negotiation := registry.existingSlot(accountID).currentState()
	negotiation.negotiationSucceeded(algorithm)
	registry.persistSharedAlgorithm(context.Background(), accountID, algorithm, snapshot)
	return negotiation
}

func publishTestIdentity(t *testing.T, registry *Registry, accountID int64, expected *sharedSnapshot, replaceable Algorithm) {
	t.Helper()
	negotiation := registry.existingSlot(accountID).currentState()
	retryAt := registry.now().Add(ambiguousIdentityRetryTTL)
	negotiation.pinIdentityUntil(retryAt)
	registry.persistSharedIdentity(context.Background(), accountID, negotiation, retryAt, replaceable, expected)
}

func TestRegistrySharedCodingRequestsSlideTTLIncludingIneligibleBodies(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 720
	publishTestAlgorithm(t, registry, id, AlgorithmZstd)
	key := sharedAlgorithmKey(id)
	for _, body := range [][]byte{testBody(), []byte("small")} {
		server.FastForward(5 * time.Hour)
		resp, err := registry.Execute(id, newTestRequest(t, body), func(req *http.Request) (*http.Response, error) {
			if len(body) >= int(MinCompressibleBodyBytes) {
				require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
			}
			return okResponse("ok"), nil
		})
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, sharedAlgorithmTTL, server.TTL(key))
		require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedRevisionKey(id)))
	}
	server.SetTTL(key, time.Hour)
	require.Equal(t, AlgorithmZstd, registry.SettledAlgorithm(context.Background(), id))
	require.Equal(t, time.Hour, server.TTL(key), "administrative reads do not renew")
}

func TestRegistryMigratesLegacySharedValues(t *testing.T) {
	for _, value := range []string{"zstd", "identity"} {
		t.Run(value, func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const id int64 = 721
			require.NoError(t, server.Set(sharedAlgorithmKey(id), value))
			snapshot := readRegistrySnapshot(t, registry, id)
			require.NotEmpty(t, snapshot.revision)
			if value == "zstd" {
				require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(id)))
				require.Equal(t, AlgorithmZstd, snapshot.owner.settledAlgorithm())
			} else {
				require.False(t, server.Exists(sharedAlgorithmKey(id)), "nonexpiring identity cannot disable recovery")
				work, owns := snapshot.owner.begin(registry.now())
				require.True(t, owns)
				require.Equal(t, TrialOrder, work)
			}
		})
	}
}

func TestSharedIdentityCASPreservesRemainingPTTLAndOtherCoding(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	registry.nowFunc = func() time.Time { return now }
	const id int64 = 722
	publishTestAlgorithm(t, registry, id, AlgorithmZstd)
	stale := readRegistrySnapshot(t, registry, id)
	publishTestIdentity(t, registry, id, stale, AlgorithmZstd)
	requireSharedAlgorithm(t, server, id, Algorithm(sharedIdentityValue))
	require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(sharedAlgorithmKey(id)))
	now = now.Add(20 * time.Minute)
	server.FastForward(20 * time.Minute)
	// The obsolete request's fresh local deadline must be replaced by Redis's
	// remaining 10 minutes, without a second non-atomic GET/PTTL round trip.
	publishTestIdentity(t, registry, id, stale, AlgorithmZstd)
	require.Equal(t, 10*time.Minute, server.TTL(sharedAlgorithmKey(id)))
	deadline, ok := stale.owner.identityRetry()
	require.True(t, ok)
	require.Equal(t, now.Add(10*time.Minute), deadline)
	// Neither a slow coding success nor another identity result can replace it.
	stale.owner.negotiationSucceeded(AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), id, AlgorithmBrotli, stale)
	requireSharedAlgorithm(t, server, id, Algorithm(sharedIdentityValue))
	require.Equal(t, 10*time.Minute, server.TTL(sharedAlgorithmKey(id)))
	require.Equal(t, AlgorithmNone, stale.owner.settledAlgorithm())
	// After expiry a fresh trial can publish a different coding. Old responses
	// cannot overwrite it or change its TTL, including a matching-coding ABA.
	now = now.Add(10 * time.Minute)
	server.FastForward(10 * time.Minute)
	publishTestAlgorithm(t, registry, id, AlgorithmZstd)
	server.SetTTL(sharedAlgorithmKey(id), time.Hour)
	publishTestIdentity(t, registry, id, stale, AlgorithmZstd)
	requireSharedAlgorithm(t, server, id, AlgorithmZstd)
	require.Equal(t, time.Hour, server.TTL(sharedAlgorithmKey(id)))
	require.Equal(t, AlgorithmZstd, stale.owner.settledAlgorithm())
}

func TestSharedExpiredIdentityCannotBeRecreatedByStaleReplayOrSuccess(t *testing.T) {
	for _, expire := range []string{"identity", "all-keys", "flush"} {
		t.Run(expire, func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const id int64 = 723
			publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
			stale := readRegistrySnapshot(t, registry, id)
			publishTestIdentity(t, registry, id, stale, AlgorithmBrotli)
			switch expire {
			case "identity":
				server.FastForward(ambiguousIdentityRetryTTL)
			case "all-keys":
				server.FastForward(sharedInvalidationTTL)
			case "flush":
				server.FlushAll()
			}
			require.False(t, server.Exists(sharedAlgorithmKey(id)))
			publishTestIdentity(t, registry, id, stale, AlgorithmBrotli)
			require.False(t, server.Exists(sharedAlgorithmKey(id)), "old replay cannot recreate identity")
			stale.owner.negotiationSucceeded(AlgorithmBrotli)
			registry.persistSharedAlgorithm(context.Background(), id, AlgorithmBrotli, stale)
			require.False(t, server.Exists(sharedAlgorithmKey(id)), "old success cannot recreate coding")
			fresh := readRegistrySnapshot(t, registry, id)
			require.NotEqual(t, stale.revision, fresh.revision)
			work, owns := fresh.owner.begin(registry.now())
			require.True(t, owns)
			require.Equal(t, TrialOrder, work)
			fresh.owner.negotiationSucceeded(AlgorithmZstd)
			registry.persistSharedAlgorithm(context.Background(), id, AlgorithmZstd, fresh)
			requireSharedAlgorithm(t, server, id, AlgorithmZstd)
		})
	}
}

func TestSharedMissingCodingCannotBeDemotedByOldReplay(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 724
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	before := readRegistrySnapshot(t, registry, id)
	server.Del(sharedAlgorithmKey(id))
	publishTestIdentity(t, registry, id, before, AlgorithmBrotli)
	require.False(t, server.Exists(sharedAlgorithmKey(id)))
	// Only a fresh initial negotiation from the missing snapshot may publish.
	fresh := readRegistrySnapshot(t, registry, id)
	publishTestIdentity(t, registry, id, fresh, AlgorithmNone)
	requireSharedAlgorithm(t, server, id, Algorithm(sharedIdentityValue))
}

func TestSharedInitialIdentityCanPublishAndRecoverAtRedisExpiry(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 725
	registry.Sync(id, true, time.Time{})
	var attempts []string
	resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		coding := req.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding != "" {
			return codedFailure(400, `{"detail":"Bad Request"}`), nil
		}
		return okResponse("plain"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br", "zstd", "deflate", ""}, attempts)
	require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(sharedAlgorithmKey(id)))
	// Local wall time passing does not expire a live Redis identity.
	registry.nowFunc = func() time.Time { return time.Now().Add(24 * time.Hour) }
	attempts = nil
	resp, err = registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, req.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{""}, attempts)
	server.FastForward(ambiguousIdentityRetryTTL)
	attempts = nil
	resp, err = registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, req.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br"}, attempts)
}

func TestSharedResetFencesInflightWritesAcrossInstances(t *testing.T) {
	for _, mutation := range []string{"identity", "success", "retire"} {
		t.Run(mutation, func(t *testing.T) {
			server, client := sharedTestRedis(t)
			a, b := sharedTestRegistry(client), sharedTestRegistry(client)
			const id int64 = 726
			publishTestAlgorithm(t, a, id, AlgorithmBrotli)
			stale := readRegistrySnapshot(t, b, id)
			a.Reset(context.Background(), id)
			resetRevision, err := server.Get(sharedRevisionKey(id))
			require.NoError(t, err)
			switch mutation {
			case "identity":
				publishTestIdentity(t, b, id, stale, AlgorithmBrotli)
			case "success":
				stale.owner.negotiationSucceeded(AlgorithmZstd)
				b.persistSharedAlgorithm(context.Background(), id, AlgorithmZstd, stale)
			case "retire":
				b.deleteSharedAlgorithm(context.Background(), id, AlgorithmBrotli, stale)
			}
			require.False(t, server.Exists(sharedAlgorithmKey(id)))
			require.True(t, server.Exists(sharedInvalidationKey(id)))
			current, err := server.Get(sharedRevisionKey(id))
			require.NoError(t, err)
			require.Equal(t, resetRevision, current)
			// A genuine post-reset probe can still publish and clear the marker.
			publishTestAlgorithm(t, a, id, AlgorithmZstd)
			requireSharedAlgorithm(t, server, id, AlgorithmZstd)
			require.False(t, server.Exists(sharedInvalidationKey(id)))
		})
	}
}

func TestSharedLocalResetRejectsDetachedStateAfterReenable(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 727
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	stale := readRegistrySnapshot(t, registry, id)
	registry.Reset(context.Background(), id)
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	server.SetTTL(sharedAlgorithmKey(id), time.Hour)
	stale.owner.negotiationSucceeded(AlgorithmBrotli)
	registry.persistSharedAlgorithm(context.Background(), id, AlgorithmBrotli, stale)
	registry.deleteSharedAlgorithm(context.Background(), id, AlgorithmBrotli, stale)
	requireSharedAlgorithm(t, server, id, AlgorithmBrotli)
	require.Equal(t, time.Hour, server.TTL(sharedAlgorithmKey(id)))
}

func TestSharedConcurrentResetAndPublicationRemainFenced(t *testing.T) {
	server, client := sharedTestRedis(t)
	a, b := sharedTestRegistry(client), sharedTestRegistry(client)
	const id int64 = 728
	for i := 0; i < 20; i++ {
		publishTestAlgorithm(t, a, id, AlgorithmBrotli)
		stale := readRegistrySnapshot(t, b, id)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); a.Reset(context.Background(), id) }()
		go func() { defer wg.Done(); publishTestIdentity(t, b, id, stale, AlgorithmBrotli) }()
		wg.Wait()
		require.False(t, server.Exists(sharedAlgorithmKey(id)))
	}
}

func TestSharedDemotionSurvivesConcurrentLocalReadBeforePublication(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 729
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	before := readRegistrySnapshot(t, registry, id)
	retryAt := registry.now().Add(ambiguousIdentityRetryTTL)
	require.True(t, before.owner.demoteSettled(AlgorithmBrotli, retryAt))
	readRegistrySnapshot(t, registry, id) // unchanged snapshot preserves pending demotion
	require.Equal(t, AlgorithmNone, before.owner.settledAlgorithm())
	registry.persistSharedIdentity(context.Background(), id, before.owner, retryAt, AlgorithmBrotli, before)
	requireSharedAlgorithm(t, server, id, Algorithm(sharedIdentityValue))
	require.Equal(t, AlgorithmNone, before.owner.settledAlgorithm())
}

func TestSharedReadFailureSendsPlainAndRecoveryUsesCurrentRedisState(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 730
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	server.SetError("ERR redis unavailable")
	for i := 0; i < 2; i++ {
		resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
			require.Empty(t, req.Header.Get("Content-Encoding"))
			return okResponse("plain"), nil
		})
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	server.SetError("")
	other := sharedTestRegistry(client)
	publishTestAlgorithm(t, other, id, AlgorithmZstd)
	resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
		return okResponse("compressed"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func TestSharedIdentityWriteFailureDoesNotKeepIndependentCooldown(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 731
	publishTestAlgorithm(t, registry, id, AlgorithmZstd)
	var attempts []string
	resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		coding := req.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding != "" {
			return codedFailure(400, `{"detail":"Bad Request"}`), nil
		}
		server.SetError("ERR write unavailable")
		return okResponse("plain"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"zstd", ""}, attempts)
	_, pinned := registry.existingSlot(id).currentState().identityRetry()
	require.False(t, pinned)
	server.SetError("")
	requireSharedAlgorithm(t, server, id, AlgorithmZstd)
	// There is no process-local 30-minute delay after Redis recovers.
	resp, err = registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
		return okResponse("compressed"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func TestSharedConcurrentReadsCoalesceAndRemainBounded(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(map[bool]string{true: "healthy", false: "outage"}[healthy], func(t *testing.T) {
			server, proxy, client := delayedSharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const id int64 = 732
			publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
			warmCount := server.CommandCount()
			readRegistrySnapshot(t, registry, id)
			commandsPerRead := server.CommandCount() - warmCount
			proxy.blocked.Store(true)
			since := proxy.upstream.Load()
			before := server.CommandCount()
			done := make(chan time.Duration, 21)
			run := func() {
				done <- elapsed(func() {
					resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
						if healthy {
							require.Equal(t, "br", req.Header.Get("Content-Encoding"))
						} else {
							require.Empty(t, req.Header.Get("Content-Encoding"))
						}
						return okResponse("ok"), nil
					})
					require.NoError(t, err)
					require.NoError(t, resp.Body.Close())
				})
			}
			go run()
			proxy.waitForUpstream(t, since)
			for i := 0; i < 20; i++ {
				go run()
			}
			// Hold the response until the burst joins the outstanding read.
			time.Sleep(40 * time.Millisecond)
			if healthy {
				proxy.blocked.Store(false)
			}
			for i := 0; i < 21; i++ {
				select {
				case d := <-done:
					require.Less(t, d, 5*sharedRefreshTimeout)
				case <-time.After(5 * sharedRefreshTimeout):
					t.Fatal("request queued behind stalled shared reads")
				}
			}
			if healthy {
				require.Equal(t, commandsPerRead, server.CommandCount()-before, "one atomic Redis read serves this healthy burst")
			}
			proxy.blocked.Store(false)
			readRegistrySnapshot(t, registry, id)
			require.Equal(t, AlgorithmBrotli, registry.existingSlot(id).currentState().settledAlgorithm())
		})
	}
}

func TestSharedRevisionAndResetMarkerAreBounded(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 733
	registry.Reset(context.Background(), id)
	require.Equal(t, sharedInvalidationTTL, server.TTL(sharedInvalidationKey(id)))
	require.Equal(t, sharedInvalidationTTL, server.TTL(sharedRevisionKey(id)))
	server.FastForward(sharedInvalidationTTL)
	require.Empty(t, server.Keys())
}

func TestClaimSharedRefreshLogRateLimitsPerAccount(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const id int64 = 730_000_001
	sharedRefreshLogTimes.Delete(id)
	sharedRefreshLogTimes.Delete(id + 1)
	require.True(t, claimSharedRefreshLog(id, now))
	require.False(t, claimSharedRefreshLog(id, now.Add(sharedRefreshLogInterval-time.Second)))
	require.True(t, claimSharedRefreshLog(id+1, now))
	require.True(t, claimSharedRefreshLog(id, now.Add(sharedRefreshLogInterval)))
}

// settleTestState bypasses shared publication for isolated logging/state tests.
func settleTestState(t *testing.T, registry *Registry, accountID int64, algorithm Algorithm) *state {
	t.Helper()
	registry.Sync(accountID, true, time.Time{})
	negotiation := registry.existingSlot(accountID).currentState()
	require.NotNil(t, negotiation)
	negotiation.negotiationSucceeded(algorithm)
	return negotiation
}

func TestSharedResetFencesStaleLocalExplicitRejections(t *testing.T) {
	_, client := sharedTestRedis(t)
	registry, other := sharedTestRegistry(client), sharedTestRegistry(client)
	const id int64 = 734
	registry.Sync(id, true, time.Time{})
	reset := false
	resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		if !reset {
			reset = true
			other.Reset(context.Background(), id)
			// The same state pointer observes the newer revision while the old
			// initial trial is still running. A tiny body starts no new trial.
			plain, err := registry.Execute(id, newTestRequest(t, []byte("small")), func(req *http.Request) (*http.Response, error) {
				require.Empty(t, req.Header.Get("Content-Encoding"))
				return okResponse("plain"), nil
			})
			require.NoError(t, err)
			require.NoError(t, plain.Body.Close())
		}
		if req.Header.Get("Content-Encoding") != "" {
			return rejectionResponse("unsupported content encoding"), nil
		}
		return okResponse("plain"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	var attempts []string
	resp, err = registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, req.Header.Get("Content-Encoding"))
		return okResponse("new trial"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br"}, attempts, "old explicit failures must not permanently pin the new generation")
}

func TestSharedSnapshotFencesEveryLocalStateTransition(t *testing.T) {
	for name, change := range map[string]func(*state, *sharedSnapshot){
		"success": func(s *state, old *sharedSnapshot) { s.negotiationSucceeded(AlgorithmDeflate, old) },
		"candidate failure": func(s *state, old *sharedSnapshot) {
			for _, coding := range TrialOrder {
				s.negotiationCandidateFailed(coding, old)
			}
		},
		"abort":         func(s *state, old *sharedSnapshot) { s.abortNegotiation(old) },
		"pin":           func(s *state, old *sharedSnapshot) { s.pinIdentity(old) },
		"temporary pin": func(s *state, old *sharedSnapshot) { s.pinIdentityUntil(time.Now().Add(time.Hour), old) },
		"retire":        func(s *state, old *sharedSnapshot) { s.retireSettled(AlgorithmBrotli, old) },
		"demote":        func(s *state, old *sharedSnapshot) { s.demoteSettled(AlgorithmBrotli, time.Now().Add(time.Hour), old) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newState()
			old := &sharedSnapshot{owner: s, revision: "old"}
			s.applyShared(old)
			_, owns := s.begin(time.Now(), old)
			require.True(t, owns)
			fresh := &sharedSnapshot{owner: s, revision: "new"}
			s.applyShared(fresh)
			_, owns = s.begin(time.Now(), fresh)
			require.True(t, owns)
			change(s, old)
			s.mu.Lock()
			require.True(t, s.negotiating, "old completion must not release the new owner")
			require.False(t, s.tested)
			require.Equal(t, AlgorithmNone, s.current)
			require.Equal(t, TrialOrder, s.remaining)
			s.mu.Unlock()
			work, owns := s.begin(time.Now(), old)
			require.Empty(t, work)
			require.False(t, owns)
		})
	}
}

func TestSharedUnchangedSnapshotPreservesRetirementOwner(t *testing.T) {
	_, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 735
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	before := readRegistrySnapshot(t, registry, id)
	work, owns := before.owner.retireSettled(AlgorithmBrotli, before)
	require.True(t, owns)
	require.Equal(t, []Algorithm{AlgorithmZstd, AlgorithmDeflate}, work)
	readRegistrySnapshot(t, registry, id)
	before.owner.mu.Lock()
	require.True(t, before.owner.negotiating)
	require.Equal(t, AlgorithmNone, before.owner.current)
	require.Equal(t, work, before.owner.remaining)
	before.owner.mu.Unlock()
	registry.deleteSharedAlgorithm(context.Background(), id, AlgorithmBrotli, before)
	readRegistrySnapshot(t, registry, id)
	before.owner.mu.Lock()
	require.True(t, before.owner.negotiating)
	require.Equal(t, work, before.owner.remaining)
	before.owner.mu.Unlock()
	before.owner.negotiationSucceeded(AlgorithmZstd, before)
	registry.persistSharedAlgorithm(context.Background(), id, AlgorithmZstd, before)
	require.Equal(t, AlgorithmZstd, before.owner.settledAlgorithm())
}

func TestSharedFailedStaleWriteDoesNotDiscardNewTrialOwner(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry, other := sharedTestRegistry(client), sharedTestRegistry(client)
	const id int64 = 736
	stale := readRegistrySnapshot(t, registry, id)
	other.Reset(context.Background(), id)
	fresh := readRegistrySnapshot(t, registry, id)
	_, owns := fresh.owner.begin(registry.now(), fresh)
	require.True(t, owns)
	server.SetError("ERR write unavailable")
	registry.persistSharedAlgorithm(context.Background(), id, AlgorithmBrotli, stale)
	fresh.owner.mu.Lock()
	require.True(t, fresh.owner.negotiating, "failed old publication cannot release the new owner")
	require.Equal(t, fresh.revision, fresh.owner.sharedRevision)
	fresh.owner.mu.Unlock()
}

func TestRegistrySettledAlgorithmReadDoesNotRenewTTL(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 737
	publishTestAlgorithm(t, registry, id, AlgorithmZstd)
	server.SetTTL(sharedAlgorithmKey(id), time.Hour)
	server.SetTTL(sharedRevisionKey(id), time.Hour)
	require.Equal(t, AlgorithmZstd, registry.SettledAlgorithm(context.Background(), id))
	require.Equal(t, time.Hour, server.TTL(sharedAlgorithmKey(id)))
	require.Equal(t, time.Hour, server.TTL(sharedRevisionKey(id)))
}

func TestSharedResetAndDisableOrderAfterInFlightRead(t *testing.T) {
	for name, clear := range map[string]func(*Registry, int64){
		"reset":   func(r *Registry, id int64) { r.Reset(context.Background(), id) },
		"disable": func(r *Registry, id int64) { r.Sync(id, false, time.Time{}) },
	} {
		t.Run(name, func(t *testing.T) {
			server, proxy, client := delayedSharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const id, otherID int64 = 738, 739
			publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
			stale := registry.existingSlot(id).currentState()
			proxy.blocked.Store(true)
			since := proxy.upstream.Load()
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				_, err := registry.syncShared(context.Background(), id, stale)
				require.ErrorIs(t, err, errSharedStateBusy)
			}()
			proxy.waitForUpstream(t, since)
			clearDone := make(chan struct{})
			go func() { defer close(clearDone); clear(registry, id) }()
			require.Eventually(t, func() bool { return !registry.Enabled(id) }, time.Second, time.Millisecond)
			select {
			case <-clearDone:
				t.Fatal("reset must wait for in-flight Redis operation")
			default:
			}
			require.Less(t, elapsed(func() { registry.Sync(otherID, true, time.Time{}); require.True(t, registry.Enabled(otherID)) }), sharedRefreshTimeout/2)
			proxy.blocked.Store(false)
			<-readDone
			select {
			case <-clearDone:
			case <-time.After(5 * time.Second):
				t.Fatal("reset did not finish")
			}
			require.False(t, server.Exists(sharedAlgorithmKey(id)))
			registry.Sync(id, true, time.Time{})
			_, err := registry.syncShared(context.Background(), id, stale)
			require.ErrorIs(t, err, errSharedStateBusy)
			require.False(t, server.Exists(sharedAlgorithmKey(id)))
		})
	}
}

func TestSharedResetLostReplyDoesNotRetryOverNewerPublication(t *testing.T) {
	server := miniredis.RunT(t)
	proxy := newDelayProxy(t, server.Addr())
	// Match production's default network retry policy rather than MaxRetries:-1.
	client := redis.NewClient(&redis.Options{Addr: proxy.listener.Addr().String(), MinRetryBackoff: time.Millisecond, MaxRetryBackoff: time.Millisecond})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	registry := sharedTestRegistry(client)
	registry.sharedWrite = client.WithTimeout(50 * time.Millisecond)
	direct := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { require.NoError(t, direct.Close()) })
	other := sharedTestRegistry(direct)
	const id int64 = 740
	registry.Reset(context.Background(), id) // preload reset script before blocking
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	proxy.blocked.Store(true)
	done := make(chan struct{})
	go func() { defer close(done); registry.Reset(context.Background(), id) }()
	require.Eventually(t, func() bool { return !server.Exists(sharedAlgorithmKey(id)) }, time.Second, time.Millisecond)
	resetRevision, err := server.Get(sharedRevisionKey(id))
	require.NoError(t, err)
	publishTestAlgorithm(t, other, id, AlgorithmZstd)
	newRevision, err := server.Get(sharedRevisionKey(id))
	require.NoError(t, err)
	require.NotEqual(t, resetRevision, newRevision)
	// Redis executed the reset, but its response is lost. Automatic go-redis
	// retries would now delete zstd and move the revision backwards to resetRevision.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reset did not respect socket timeout")
	}
	proxy.blocked.Store(false)
	requireSharedAlgorithm(t, server, id, AlgorithmZstd)
	currentRevision, err := server.Get(sharedRevisionKey(id))
	require.NoError(t, err)
	require.Equal(t, newRevision, currentRevision)
}

func TestSharedRetirementWriteFailureCannotPinLocalIdentity(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const id int64 = 741
	publishTestAlgorithm(t, registry, id, AlgorithmZstd)
	resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Content-Encoding") != "" {
			server.SetError("ERR retirement write unavailable")
			return rejectionResponse("unsupported content encoding"), nil
		}
		return okResponse("plain"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	server.SetError("")
	requireSharedAlgorithm(t, server, id, AlgorithmZstd)
	resp, err = registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
		return okResponse("authoritative coding"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func TestDisablingRedisRestoresBoundedMemoryOnlyCooldown(t *testing.T) {
	_, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	registry.nowFunc = func() time.Time { return now }
	const id int64 = 742
	publishTestAlgorithm(t, registry, id, AlgorithmBrotli)
	before := readRegistrySnapshot(t, registry, id)
	publishTestIdentity(t, registry, id, before, AlgorithmBrotli)
	registry.SetRedis(nil)
	now = now.Add(ambiguousIdentityRetryTTL)
	resp, err := registry.Execute(id, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "br", req.Header.Get("Content-Encoding"))
		return okResponse("local recovery"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}
