package reqcompress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// codingSensitiveFailures are coded-attempt failures the content coding may
// have caused (docs/REQUEST_COMPRESSION.md). The first entry is the production
// ChatGPT Codex rejection that only zstd-coded request bodies received.
var codingSensitiveFailures = []struct {
	status int
	body   string
	kind   string
}{
	{400, `{"detail":"Bad Request"}`, responseFailureGenericBadRequest},
	{400, `{"error":{"message":"Failed to read request body","type":"invalid_request_error"}}`, responseFailureBodyRead},
	{400, `{"error":{"message":"Failed to decode JSON request body"}}`, responseFailureHTTP},
	{400, `{"input":"unsupported brotli","error":{"message":"invalid model"}}`, responseFailureHTTP},
	{413, `{"error":{"message":"request entity too large"}}`, responseFailureHTTP},
	{415, `{"error":{"message":"unsupported media type application/json"}}`, responseFailureMediaType},
	{415, "", responseFailureMediaType},
	{422, `{"error":{"message":"invalid model zstd"}}`, responseFailureHTTP},
	{500, `{"error":"temporary failure"}`, responseFailureHTTP},
	{500, `<html>failed to decompress request body in an unrelated diagnostic</html>`, responseFailureHTTP},
	{501, `{"error":"not implemented"}`, responseFailureHTTP},
}

// settleBrotli enables accountID and settles it on the first trial coding
// through an ordinary successful request.
func settleBrotli(t *testing.T, registry *Registry, accountID int64) {
	t.Helper()
	registry.Sync(accountID, true, time.Time{})
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
		return okResponse("settled"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
}

func codedFailure(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

// docs/REQUEST_COMPRESSION.md: a settled coding whose attempt fails with a
// coding-sensitive status is replayed once without content coding. The caller
// only receives the plain response, the discarded coded attempt stays out of
// the usage record, and a plain success pins identity for 30 minutes (also in
// Redis) before the failed coding is probed again.
func TestExecuteReplaysSettledCodingFailureWithoutCoding(t *testing.T) {
	for _, tc := range codingSensitiveFailures {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.body), func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			now := time.Date(2026, 11, 10, 11, 30, 0, 0, time.UTC)
			registry.nowFunc = func() time.Time { return now }
			const accountID int64 = 821
			settleBrotli(t, registry, accountID)

			logs := captureLogs(t)
			collector := traffic.NewCollector()
			ctx := context.WithValue(context.Background(), ctxkey.RequestID, "local-request-821")
			ctx = traffic.WithCollector(ctx, collector)
			body := testBody()
			var attempts []string
			resp, err := registry.Execute(accountID, newTestRequest(t, body).WithContext(ctx), func(attempt *http.Request) (*http.Response, error) {
				coding := attempt.Header.Get("Content-Encoding")
				attempts = append(attempts, coding)
				probe := traffic.FromContext(attempt.Context())
				if coding != "" {
					require.Equal(t, "br", coding)
					require.Equal(t, body, decodeRequest(t, attempt))
					require.NotSame(t, collector, probe, "the coded attempt is measured apart")
					counter := probe.Begin(traffic.UpstreamRequest, true)
					counter.SetOriginal(int64(len(body)))
					counter.AddActual(100)
					counter.CompleteActual()
					failure := codedFailure(tc.status, tc.body)
					failure.Header = http.Header{"X-Request-Id": {"coded-request-821"}}
					return failure, nil
				}
				require.Same(t, collector, probe, "the plain replay is the delivered request")
				require.Equal(t, string(body), readAll(t, attempt.Body), "the replay carries the original entity")
				counter := probe.Begin(traffic.UpstreamRequest, false)
				counter.AddOriginal(len(body))
				counter.AddActual(len(body))
				counter.CompleteOriginal()
				counter.CompleteActual()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"X-Oai-Request-Id": {"plain-request-821"}},
					Body:       io.NopCloser(strings.NewReader("plain")),
				}, nil
			})
			require.NoError(t, err)
			require.Equal(t, []string{"br", ""}, attempts)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "plain", readAll(t, resp.Body))
			require.NoError(t, resp.Body.Close())

			collector.Finalize()
			usage := collector.Snapshot()
			require.NotNil(t, usage.Upstream.Request)
			require.False(t, usage.Upstream.Request.Compressed, "only the delivered plain attempt is recorded")
			require.EqualValues(t, len(body), *usage.Upstream.Request.ActualBytes)

			out := logs.String()
			require.Contains(t, out, `level=WARN msg="reqcompress identity replay recovered"`)
			for _, field := range []string{
				"request_id=local-request-821", "algorithm=br", fmt.Sprintf("status=%d", tc.status),
				"upstream_request_id=coded-request-821", "reason=" + tc.kind, "identity_retry=true",
				"identity_status=200", "identity_upstream_request_id=plain-request-821",
				"identity_pinned=true", "identity_ttl_seconds=1800", "sent_bytes=100",
			} {
				require.Contains(t, out, field)
			}
			if tc.body != "" {
				require.NotContains(t, out, tc.body, "error bodies may echo private content")
			}

			requireSharedAlgorithm(t, server, accountID, Algorithm(sharedIdentityValue))
			require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(sharedAlgorithmKey(accountID)))
			require.Equal(t, AlgorithmNone, registry.SettledAlgorithm(ctx, accountID))

			// Within the TTL requests go out plain without another coded attempt.
			now = now.Add(ambiguousIdentityRetryTTL - time.Second)
			server.FastForward(ambiguousIdentityRetryTTL - time.Second)
			attempts = nil
			resp, err = registry.Execute(accountID, newTestRequest(t, body), func(attempt *http.Request) (*http.Response, error) {
				attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
				return okResponse("plain"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []string{""}, attempts)

			// At the TTL boundary the failed coding leads a new trial and, once
			// accepted, settles again with the ordinary sliding TTL.
			now = now.Add(time.Second)
			server.FastForward(time.Second)
			attempts = nil
			resp, err = registry.Execute(accountID, newTestRequest(t, body), func(attempt *http.Request) (*http.Response, error) {
				attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
				return okResponse("recovered"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []string{"br"}, attempts)
			requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
			require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
		})
	}
}

// docs/REQUEST_COMPRESSION.md: when the plain replay fails as well, the coding
// did not cause the failure. The plain response is delivered and the coding
// stays settled for later requests.
func TestExecuteReplayFailureKeepsSettledCoding(t *testing.T) {
	for _, tc := range codingSensitiveFailures {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.body), func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const accountID int64 = 822
			settleBrotli(t, registry, accountID)
			server.FastForward(time.Hour)

			logs := captureLogs(t)
			var attempts []string
			bothFail := func(attempt *http.Request) (*http.Response, error) {
				coding := attempt.Header.Get("Content-Encoding")
				attempts = append(attempts, coding)
				if coding != "" {
					return codedFailure(tc.status, tc.body), nil
				}
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"X-Request-Id": {"plain-request-822"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"plain failure"}}`)),
				}, nil
			}
			resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), bothFail)
			require.NoError(t, err)
			require.Equal(t, []string{"br", ""}, attempts)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.Equal(t, "plain failure", errorMessage(t, resp.Body), "the caller receives the plain outcome")

			out := logs.String()
			require.Contains(t, out, `level=WARN msg="reqcompress identity replay failed"`)
			require.Contains(t, out, "identity_status=400")
			require.Contains(t, out, "identity_upstream_request_id=plain-request-822")
			require.Contains(t, out, "coding_preserved=true")
			require.NotContains(t, out, "reqcompress identity replay recovered")
			requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
			require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))

			// An ordinary invalid request fails both ways on every retry, so the
			// warning is rate limited per account while Debug records each event.
			attempts = nil
			resp, err = registry.Execute(accountID, newTestRequest(t, testBody()), bothFail)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []string{"br", ""}, attempts, "every coded failure still gets one plain replay")
			out = logs.String()
			require.Equal(t, 1, strings.Count(out, `level=WARN msg="reqcompress identity replay failed"`))
			require.Equal(t, 1, strings.Count(out, `level=DEBUG msg="reqcompress identity replay failed"`))

			attempts = nil
			resp, err = registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
				attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
				return okResponse("still compressed"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, []string{"br"}, attempts)
		})
	}
}

func TestExecuteReplayTransportFailureKeepsSettledCoding(t *testing.T) {
	logs := captureLogs(t)
	registry := NewRegistry()
	const accountID int64 = 823
	settleBrotli(t, registry, accountID)

	var attempts []string
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		coding := attempt.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding != "" {
			return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
		}
		return nil, io.ErrUnexpectedEOF
	})
	require.Nil(t, resp)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF, "the plain outcome is delivered")
	require.Equal(t, []string{"br", ""}, attempts)
	require.Contains(t, logs.String(), `msg="reqcompress identity replay failed"`)
	require.Contains(t, logs.String(), "identity_error_kind=eof")
	require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
}

func TestExecuteDoesNotReplayForCanceledCaller(t *testing.T) {
	registry := NewRegistry()
	const accountID int64 = 824
	settleBrotli(t, registry, accountID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts []string
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()).WithContext(ctx), func(attempt *http.Request) (*http.Response, error) {
		attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
		cancel() // the client went away while the coded attempt was in flight
		return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"br"}, attempts, "nobody can receive a replay")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
}

func TestStateDemoteSettledOnlyReplacesTheFailedSettledCoding(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := newState()
	require.False(t, s.demoteSettled(AlgorithmZstd, now.Add(time.Minute)), "an unsettled trial is not demoted")

	s.negotiationSucceeded(AlgorithmZstd)
	require.False(t, s.demoteSettled(AlgorithmBrotli, now.Add(time.Minute)), "a different settled coding is kept")
	require.True(t, s.demoteSettled(AlgorithmZstd, now.Add(time.Minute)))
	require.False(t, s.demoteSettled(AlgorithmZstd, now.Add(2*time.Minute)), "a concurrent replay keeps the first deadline")
	retryAt, ok := s.identityRetry()
	require.True(t, ok)
	require.Equal(t, now.Add(time.Minute), retryAt)

	work, owns := s.begin(now)
	require.Empty(t, work)
	require.False(t, owns)
	work, owns = s.begin(now.Add(time.Minute))
	require.True(t, owns)
	require.Equal(t, []Algorithm{AlgorithmZstd, AlgorithmDeflate}, work, "the failed coding leads the new trial")
}

func TestPersistSharedIdentityScriptReplacesOnlyTheFailedCoding(t *testing.T) {
	server, client := sharedTestRedis(t)
	const accountID int64 = 713
	key := sharedAlgorithmKey(accountID)
	run := func(replaceable Algorithm) {
		t.Helper()
		require.NoError(t, persistSharedIdentityScript.Run(context.Background(), client,
			[]string{key, sharedInvalidationKey(accountID)}, ambiguousIdentityRetryTTL.Milliseconds(), string(replaceable)).Err())
	}

	// A different coding settled by another instance survives a zstd demotion.
	require.NoError(t, server.Set(key, string(AlgorithmDeflate)))
	server.SetTTL(key, sharedAlgorithmTTL)
	run(AlgorithmZstd)
	requireSharedAlgorithm(t, server, accountID, AlgorithmDeflate)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(key))

	// The failed coding itself becomes a fixed 30-minute identity.
	require.NoError(t, server.Set(key, string(AlgorithmZstd)))
	server.SetTTL(key, sharedAlgorithmTTL)
	run(AlgorithmZstd)
	requireSharedAlgorithm(t, server, accountID, Algorithm(sharedIdentityValue))
	require.Equal(t, ambiguousIdentityRetryTTL, server.TTL(key))

	// Later demotions never extend the shared identity deadline.
	server.FastForward(10 * time.Minute)
	run(AlgorithmZstd)
	require.Equal(t, 20*time.Minute, server.TTL(key))
}

// docs/REQUEST_COMPRESSION.md: the demotion is shared. A fresh instance
// inherits the remaining identity deadline; an instance that still holds the
// coding locally keeps using it until its own replay, and its renewals cannot
// overwrite or extend the shared identity.
func TestRegistrySharesIdentityDemotionAcrossInstances(t *testing.T) {
	server, client := sharedTestRedis(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	first := sharedTestRegistry(client)
	stale := sharedTestRegistry(client)
	fresh := sharedTestRegistry(client)
	first.nowFunc, stale.nowFunc, fresh.nowFunc = clock, clock, clock
	const accountID int64 = 714
	key := sharedAlgorithmKey(accountID)
	settleBrotli(t, first, accountID)
	stale.Sync(accountID, true, time.Time{})
	compressedOK := func(attempt *http.Request) (*http.Response, error) {
		require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	}
	resp, err := stale.Execute(accountID, newTestRequest(t, testBody()), compressedOK)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	resp, err = first.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		if attempt.Header.Get("Content-Encoding") != "" {
			return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
		}
		return okResponse("plain"), nil
	})
	require.NoError(t, err)
	require.Equal(t, "plain", readAll(t, resp.Body))
	require.NoError(t, resp.Body.Close())
	requireSharedAlgorithm(t, server, accountID, Algorithm(sharedIdentityValue))

	deadline := now.Add(ambiguousIdentityRetryTTL)
	now = now.Add(10 * time.Minute)
	server.FastForward(10 * time.Minute)
	resp, err = stale.Execute(accountID, newTestRequest(t, testBody()), compressedOK)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	requireSharedAlgorithm(t, server, accountID, Algorithm(sharedIdentityValue))
	require.Equal(t, 20*time.Minute, server.TTL(key))

	fresh.Sync(accountID, true, time.Time{})
	resp, err = fresh.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		require.Empty(t, attempt.Header.Get("Content-Encoding"))
		return okResponse("plain"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	retryAt, ok := fresh.existingSlot(accountID).currentState().identityRetry()
	require.True(t, ok)
	require.Equal(t, deadline, retryAt, "instances share one identity deadline")
}
