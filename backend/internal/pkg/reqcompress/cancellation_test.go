package reqcompress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// The gateway preserves the upstream attempt for billing after the client
// disconnects. That detached context must not authorize a new paid request.
func TestExecuteSettledCodingDoesNotReplayAfterDetachedCallerDisconnect(t *testing.T) {
	registry := NewRegistry()
	const accountID int64 = 9824001
	settleBrotli(t, registry, accountID)

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstreamCtx := pkghttputil.DetachUpstreamContext(clientCtx)
	req := newTestRequest(t, testBody()).WithContext(upstreamCtx)
	var attempts []string
	resp, err := registry.Execute(accountID, req, func(attempt *http.Request) (*http.Response, error) {
		coding := attempt.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding != "" {
			// The client goes away while its coded attempt is in flight. The
			// gateway can still finish this response to collect usage.
			cancelClient()
			require.ErrorIs(t, clientCtx.Err(), context.Canceled)
			require.NoError(t, attempt.Context().Err(), "billing context is intentionally detached")
			return codedFailure(http.StatusBadRequest, `{"detail":"Bad Request"}`), nil
		}
		return okResponse("unexpected new request after disconnection"), nil
	})
	if resp != nil && resp.Body != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	require.NoError(t, err)
	require.Equal(t, []string{"br"}, attempts, "drain the existing attempt but never replay for a disconnected client")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
}

func TestExecuteDetachedCancellationKeepsResponseAndTraffic(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, duringInspection := range []bool{false, true} {
			for _, readFailure := range []bool{false, true} {
				t.Run(fmt.Sprintf("initial=%t/inspection=%t/read_failure=%t", initial, duringInspection, readFailure), func(t *testing.T) {
					registry := NewRegistry()
					const accountID int64 = 9824002
					if initial {
						registry.Sync(accountID, true, time.Time{})
					} else {
						settleBrotli(t, registry, accountID)
					}
					collector := traffic.NewCollector()
					client, cancel := context.WithCancel(context.Background())
					defer cancel()
					ctx := traffic.WithCollector(pkghttputil.DetachUpstreamContext(client), collector)
					payload := `{"detail":"Bad Request"}`
					reader := io.Reader(strings.NewReader(payload))
					if readFailure {
						reader = io.MultiReader(reader, &replayReadError{err: io.ErrUnexpectedEOF})
					}
					body := &cancellationResponseBody{reader: reader}
					if duringInspection {
						body.onRead = cancel
					}
					var attempts []string
					resp, err := registry.Execute(accountID, newTestRequest(t, testBody()).WithContext(ctx), func(attempt *http.Request) (*http.Response, error) {
						attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
						if !duringInspection {
							cancel()
						}
						return &http.Response{
							StatusCode: http.StatusBadRequest,
							Header:     http.Header{"X-Request-Id": {"original-attempt"}},
							Body:       measureCancellationAttempt(attempt, body),
						}, nil
					})
					require.NoError(t, err)
					require.Equal(t, []string{"br"}, attempts)
					require.ErrorIs(t, client.Err(), context.Canceled)
					require.NoError(t, ctx.Err(), "billing context is still live")
					require.Equal(t, http.StatusBadRequest, resp.StatusCode)
					require.Equal(t, "original-attempt", resp.Header.Get("X-Request-Id"))
					require.False(t, body.closed, "the retained response must stay readable")
					got, readErr := io.ReadAll(resp.Body)
					if readFailure {
						require.ErrorIs(t, readErr, io.ErrUnexpectedEOF)
					} else {
						require.NoError(t, readErr)
					}
					require.Equal(t, payload, string(got))
					require.NoError(t, resp.Body.Close())
					require.True(t, body.closed)
					requireCancellationTraffic(t, collector, len(payload))
					require.False(t, registry.existingSlot(accountID).currentState().negotiating)
					if !initial {
						require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
					}
				})
			}
		}
	}
}

func TestExecuteDetachedCancellationDuringDiscardPreventsRetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial bool
		status  int
		payload string
		onRead  bool
	}{
		{"settled_plain_replay_drain", false, 413, "too large", true},
		{"settled_plain_replay_close", false, 400, `{"detail":"Bad Request"}`, false},
		{"initial_trial_drain", true, 413, "too large", true},
		{"initial_trial_close", true, 400, `{"detail":"Bad Request"}`, false},
		{"settled_rejection_close", false, 415, "unsupported content encoding", false},
		{"initial_rejection_close", true, 415, "unsupported content encoding", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry()
			const accountID int64 = 9824003
			if tc.initial {
				registry.Sync(accountID, true, time.Time{})
			} else {
				settleBrotli(t, registry, accountID)
			}
			collector := traffic.NewCollector()
			client, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := traffic.WithCollector(pkghttputil.DetachUpstreamContext(client), collector)
			body := &cancellationResponseBody{reader: strings.NewReader(tc.payload)}
			if tc.onRead {
				body.onRead = cancel
			} else {
				body.onClose = cancel
			}
			var attempts []string
			resp, err := registry.Execute(accountID, newTestRequest(t, testBody()).WithContext(ctx), func(attempt *http.Request) (*http.Response, error) {
				attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
				return &http.Response{StatusCode: tc.status, Body: measureCancellationAttempt(attempt, body)}, nil
			})
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, resp, "never return an already consumed/closed response")
			require.Equal(t, []string{"br"}, attempts)
			require.True(t, body.closed, "the discarded response still releases its attempt")
			require.NoError(t, ctx.Err(), "do not cancel the detached billing context")
			requireCancellationTraffic(t, collector, len(tc.payload))
			require.False(t, registry.existingSlot(accountID).currentState().negotiating)
			if !tc.initial {
				require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
			}
			// Cancellation must release trial ownership for a later live caller.
			resp, err = registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
				require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
				return okResponse("live caller"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestExecuteDetachedCancellationDuringNextEncodingPreventsRetry(t *testing.T) {
	for _, encodeFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("encode_failure=%t", encodeFailure), func(t *testing.T) {
			registry := NewRegistry()
			const accountID int64 = 9824004
			registry.Sync(accountID, true, time.Time{})
			collector := traffic.NewCollector()
			client, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := traffic.WithCollector(pkghttputil.DetachUpstreamContext(client), collector)
			req := newTestRequest(t, testBody()).WithContext(ctx)
			getBody := req.GetBody
			encodes := 0
			req.GetBody = func() (io.ReadCloser, error) {
				encodes++
				if encodes == 2 {
					cancel()
					if encodeFailure {
						return nil, io.ErrUnexpectedEOF
					}
				}
				return getBody()
			}
			payload := "unsupported content encoding"
			body := &cancellationResponseBody{reader: strings.NewReader(payload)}
			var attempts []string
			resp, err := registry.Execute(accountID, req, func(attempt *http.Request) (*http.Response, error) {
				attempts = append(attempts, attempt.Header.Get("Content-Encoding"))
				return &http.Response{StatusCode: 415, Body: measureCancellationAttempt(attempt, body)}, nil
			})
			require.ErrorIs(t, err, context.Canceled)
			require.Nil(t, resp)
			require.Equal(t, []string{"br"}, attempts)
			require.True(t, body.closed)
			requireCancellationTraffic(t, collector, len(payload))
			require.False(t, registry.existingSlot(accountID).currentState().negotiating)
		})
	}
}

func TestExecuteDetachedCancellationBeforeFinalIdentityPreventsRetry(t *testing.T) {
	registry := NewRegistry()
	const accountID int64 = 9824006
	registry.Sync(accountID, true, time.Time{})
	collector := traffic.NewCollector()
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := traffic.WithCollector(pkghttputil.DetachUpstreamContext(client), collector)
	payload := `{"detail":"Bad Request"}`
	var attempts []string
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()).WithContext(ctx), func(attempt *http.Request) (*http.Response, error) {
		coding := attempt.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		body := &cancellationResponseBody{reader: strings.NewReader(payload)}
		if coding == string(AlgorithmDeflate) {
			body.onClose = cancel
		}
		return &http.Response{StatusCode: 400, Body: measureCancellationAttempt(attempt, body)}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, resp)
	require.Equal(t, []string{"br", "zstd", "deflate"}, attempts, "do not send the final plain attempt after cancellation")
	requireCancellationTraffic(t, collector, len(payload))
	negotiation := registry.existingSlot(accountID).currentState()
	require.False(t, negotiation.negotiating)
	require.False(t, negotiation.tested, "cancellation does not pin identity")
}

func TestExecuteLiveDetachedCallerStillRecoversAndDrains(t *testing.T) {
	registry := NewRegistry()
	const accountID int64 = 9824005
	settleBrotli(t, registry, accountID)
	client, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := pkghttputil.DetachUpstreamContext(client)
	var attempts []string
	var upstream context.Context
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()).WithContext(ctx), func(attempt *http.Request) (*http.Response, error) {
		coding := attempt.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding != "" {
			return codedFailure(400, `{"detail":"Bad Request"}`), nil
		}
		var release context.CancelFunc
		upstream, release = pkghttputil.NewUpstreamAttemptContext(attempt.Context(), time.Second)
		return &http.Response{StatusCode: 200, Body: &cancellationResponseBody{reader: strings.NewReader("late usage"), onClose: release}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"br", ""}, attempts, "live clients retain the single plain recovery")
	require.Equal(t, AlgorithmNone, registry.existingSlot(accountID).currentState().settledAlgorithm())
	cancel()
	require.ErrorIs(t, pkghttputil.UpstreamClientErr(upstream), context.Canceled)
	require.NoError(t, upstream.Err(), "the replay already in flight keeps its billing drain")
	require.Equal(t, "late usage", readAll(t, resp.Body))
	require.NoError(t, resp.Body.Close())
	require.ErrorIs(t, upstream.Err(), context.Canceled)
}

func TestExecuteDetachedHTTPResponseCompletesWithoutReplayAfterDisconnect(t *testing.T) {
	registry := NewRegistry()
	const accountID int64 = 9824007
	settleBrotli(t, registry, accountID)
	client, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	const payload = `{"detail":"Bad Request"}`
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		attempts.Add(1)
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("X-Request-Id", "existing-upstream-attempt")
		w.WriteHeader(http.StatusBadRequest)
		_ = http.NewResponseController(w).Flush()
		cancelClient()
		// The existing response still reaches inspection after disconnection.
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 2 * time.Second
	req, err := http.NewRequestWithContext(pkghttputil.DetachUpstreamContext(client), http.MethodPost, server.URL, strings.NewReader(string(testBody())))
	require.NoError(t, err)
	var upstream context.Context
	resp, err := registry.Execute(accountID, req, func(attempt *http.Request) (*http.Response, error) {
		var release context.CancelFunc
		upstream, release = pkghttputil.NewUpstreamAttemptContext(attempt.Context(), time.Second)
		response, sendErr := httpClient.Do(attempt.WithContext(upstream))
		if sendErr != nil {
			release()
			return response, sendErr
		}
		original := response.Body
		response.Body = &cancellationResponseBody{reader: original, onClose: func() {
			release()
			_ = original.Close()
		}}
		return response, nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.EqualValues(t, 1, attempts.Load())
	require.ErrorIs(t, client.Err(), context.Canceled)
	require.NoError(t, upstream.Err(), "reading the existing response retains the drain budget")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "existing-upstream-attempt", resp.Header.Get("X-Request-Id"))
	require.Equal(t, payload, readAll(t, resp.Body))
	require.NoError(t, resp.Body.Close())
	require.ErrorIs(t, upstream.Err(), context.Canceled)
	require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
}

type cancellationResponseBody struct {
	reader  io.Reader
	onRead  func()
	onClose func()
	closed  bool
}

func (b *cancellationResponseBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	if b.onRead != nil {
		callback := b.onRead
		b.onRead = nil
		callback()
	}
	return b.reader.Read(p)
}

func (b *cancellationResponseBody) Close() error {
	if !b.closed {
		b.closed = true
		if b.onClose != nil {
			b.onClose()
		}
	}
	return nil
}

func measureCancellationAttempt(req *http.Request, body io.ReadCloser) io.ReadCloser {
	probe := traffic.FromContext(req.Context())
	request := probe.Begin(traffic.UpstreamRequest, true)
	request.SetOriginal(int64(len(testBody())))
	request.AddActual(100)
	request.CompleteActual()
	return traffic.ObserveResponseBody(body, probe.Begin(traffic.UpstreamResponse, false), true, -1)
}

func requireCancellationTraffic(t *testing.T, collector *traffic.Collector, responseBytes int) {
	t.Helper()
	collector.Finalize()
	usage := collector.Snapshot()
	require.NotNil(t, usage.Upstream.Request)
	require.True(t, usage.Upstream.Request.Compressed)
	require.EqualValues(t, len(testBody()), *usage.Upstream.Request.OriginalBytes)
	require.EqualValues(t, 100, *usage.Upstream.Request.ActualBytes)
	require.NotNil(t, usage.Upstream.Response)
	require.EqualValues(t, responseBytes, *usage.Upstream.Response.OriginalBytes)
	require.EqualValues(t, responseBytes, *usage.Upstream.Response.ActualBytes, "inspection/restoration must not count reads twice")
}
