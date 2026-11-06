package reqcompress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// docs/REQUEST_COMPRESSION.md: a verified coding is a capability, not a health
// check. Ordinary HTTP errors (including the production body-read diagnostic)
// cannot invalidate it or trigger an uncompressed POST replay.
func TestExecutePreservesSettledCodingOnOrdinaryHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   string
	}{
		{400, `{"error":{"message":"Failed to read request body","type":"invalid_request_error"}}`, responseFailureBodyRead},
		{400, `{"error":{"message":"Failed to decode JSON request body"}}`, responseFailureHTTP},
		{400, `{"detail":"Bad Request"}`, responseFailureGenericBadRequest},
		{400, `{"input":"unsupported brotli","error":{"message":"invalid model"}}`, responseFailureHTTP},
		{400, `{"error":{"message":"failed to decompress image attachment"}}`, responseFailureHTTP},
		{500, `<html>failed to decompress request body in an unrelated diagnostic</html>`, responseFailureHTTP},
		{500, `{"error":{"message":"failed to decode upstream response content-encoding"}}`, responseFailureHTTP},
		{401, `{"error":{"message":"invalid token"}}`, responseFailureHTTP},
		{403, `{"error":"rejected request"}`, responseFailureHTTP},
		{408, `{"error":"timeout"}`, responseFailureHTTP},
		{415, `{"error":{"message":"unsupported media type application/json"}}`, responseFailureMediaType},
		{415, "", responseFailureMediaType},
		{422, `{"error":{"message":"invalid model zstd"}}`, responseFailureHTTP},
		{429, `{"error":"rate limited"}`, responseFailureHTTP},
		{500, `{"error":"temporary failure"}`, responseFailureHTTP},
		{502, `{"error":"bad gateway"}`, responseFailureHTTP},
		{503, `{"error":"unavailable"}`, responseFailureHTTP},
		{504, `{"error":"gateway timeout"}`, responseFailureHTTP},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.body), func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const accountID int64 = 811
			registry.Sync(accountID, true, time.Time{})
			resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(*http.Request) (*http.Response, error) {
				return okResponse("settled"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			server.FastForward(time.Hour)

			logs := captureLogs(t)
			collector := traffic.NewCollector()
			ctx := context.WithValue(context.Background(), ctxkey.RequestID, "local-request-811")
			ctx = traffic.WithCollector(ctx, collector)
			req := newTestRequest(t, testBody()).WithContext(ctx)
			attempts := 0
			resp, err = registry.Execute(accountID, req, func(attempt *http.Request) (*http.Response, error) {
				attempts++
				require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
				require.Equal(t, testBody(), decodeRequest(t, attempt))
				probe := traffic.FromContext(attempt.Context())
				counter := probe.Begin(traffic.UpstreamRequest, true)
				counter.SetOriginal(int64(len(testBody())))
				counter.AddActual(100)
				counter.CompleteActual()
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"X-Request-Id": {"upstream-request-811"}},
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})
			require.NoError(t, err)
			require.Equal(t, 1, attempts, "no uncompressed retry may amplify traffic")
			require.Equal(t, tc.status, resp.StatusCode)
			require.Equal(t, tc.body, readAll(t, resp.Body), "diagnostic inspection must restore the response")
			require.NoError(t, resp.Body.Close())
			requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
			require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
			require.Equal(t, AlgorithmBrotli, registry.SettledAlgorithm(ctx, accountID))

			collector.Finalize()
			usage := collector.Snapshot()
			require.NotNil(t, usage.Upstream.Request, "delivered error attempts still count as traffic")
			require.True(t, usage.Upstream.Request.Compressed)
			require.EqualValues(t, 100, *usage.Upstream.Request.ActualBytes)
			require.Contains(t, logs.String(), "request_id=local-request-811")
			require.Contains(t, logs.String(), "upstream_request_id=upstream-request-811")
			require.Contains(t, logs.String(), "reason="+tc.kind)
			require.Contains(t, logs.String(), "coding_preserved=true")
			if tc.body != "" {
				require.NotContains(t, logs.String(), tc.body)
			}
			require.NotContains(t, logs.String(), "reqcompress pinned identity")

			resp, err = registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
				require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
				return okResponse("still compressed"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestExecutePreservesSettledCodingOnTransportFailure(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, syscall.EPIPE, syscall.ECONNRESET, io.ErrUnexpectedEOF, io.ErrClosedPipe} {
		t.Run(failure.Error(), func(t *testing.T) {
			server, client := sharedTestRedis(t)
			registry := sharedTestRegistry(client)
			const accountID int64 = 812
			registry.Sync(accountID, true, time.Time{})
			resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(*http.Request) (*http.Response, error) {
				return okResponse("settled"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			server.FastForward(time.Hour)
			attempts := 0
			req := newTestRequest(t, testBody())
			if errors.Is(failure, context.Canceled) {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			_, err = registry.Execute(accountID, req, func(attempt *http.Request) (*http.Response, error) {
				attempts++
				require.Equal(t, "br", attempt.Header.Get("Content-Encoding"))
				return nil, failure
			})
			require.ErrorIs(t, err, failure)
			require.Equal(t, 1, attempts)
			requireSharedAlgorithm(t, server, accountID, AlgorithmBrotli)
			require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
			require.Equal(t, AlgorithmBrotli, registry.existingSlot(accountID).currentState().settledAlgorithm())
		})
	}
}

func TestExecuteRefreshesSharedCodingForEveryAccountRequest(t *testing.T) {
	server, client := sharedTestRedis(t)
	registry := sharedTestRegistry(client)
	const accountID int64 = 813
	registry.Sync(accountID, true, time.Time{})
	// A fresh process must also refresh a legacy, non-expiring shared result on
	// its very first small/ineligible request, not only on compressed bodies.
	require.NoError(t, server.Set(sharedAlgorithmKey(accountID), "zstd"))
	for _, payload := range [][]byte{[]byte("tiny"), testBody()} {
		server.FastForward(time.Hour)
		resp, err := registry.Execute(accountID, newTestRequest(t, payload), func(attempt *http.Request) (*http.Response, error) {
			if len(payload) < int(MinCompressibleBodyBytes) {
				require.Empty(t, attempt.Header.Get("Content-Encoding"))
			} else {
				require.Equal(t, "zstd", attempt.Header.Get("Content-Encoding"))
			}
			return okResponse("ok"), nil
		})
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
	}

	// Losing the shared key must not leave the UI empty indefinitely while
	// in-memory forwarding keeps compressing; the next account request repairs it.
	require.NoError(t, client.Del(context.Background(), sharedAlgorithmKey(accountID)).Err())
	resp, err := registry.Execute(accountID, newTestRequest(t, testBody()), func(attempt *http.Request) (*http.Response, error) {
		require.Equal(t, "zstd", attempt.Header.Get("Content-Encoding"))
		return okResponse("ok"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	requireSharedAlgorithm(t, server, accountID, AlgorithmZstd)
	require.Equal(t, sharedAlgorithmTTL, server.TTL(sharedAlgorithmKey(accountID)))
}

func TestExecuteInitialHealthFailuresDoNotMultiplyTrafficOrPinIdentity(t *testing.T) {
	for _, status := range []int{400, 403, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			registry := NewRegistry()
			registry.Sync(814, true, time.Time{})
			attempts := 0
			resp, err := registry.Execute(814, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
				attempts++
				require.Equal(t, "br", req.Header.Get("Content-Encoding"))
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":"temporary failure"}`))}, nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, 1, attempts)
			resp, err = registry.Execute(814, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "br", req.Header.Get("Content-Encoding"))
				return okResponse("recovered"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, AlgorithmBrotli, registry.SettledAlgorithm(context.Background(), 814))
		})
	}
}

func TestExecuteRetiredCodingHealthFailureKeepsRemainingCompressedCandidates(t *testing.T) {
	registry := NewRegistry()
	registry.Sync(815, true, time.Time{})
	resp, err := registry.Execute(815, newTestRequest(t, testBody()), func(*http.Request) (*http.Response, error) {
		return okResponse("settled"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	var attempts []string
	resp, err = registry.Execute(815, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		coding := req.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding == "br" {
			return rejectionResponse("unsupported content encoding"), nil
		}
		return &http.Response{StatusCode: 502, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br", "zstd"}, attempts)
	resp, err = registry.Execute(815, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
		return okResponse("recovered"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, AlgorithmZstd, registry.SettledAlgorithm(context.Background(), 815))
}

func TestExecuteInitialBodyReadFailureOnlyPinsIdentityAfterSuccessfulPlainAttempt(t *testing.T) {
	for _, result := range []string{"success", "http_failure", "transport_failure"} {
		t.Run(result, func(t *testing.T) {
			registry := NewRegistry()
			registry.Sync(816, true, time.Time{})
			var attempts []string
			resp, err := registry.Execute(816, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
				coding := req.Header.Get("Content-Encoding")
				attempts = append(attempts, coding)
				if coding != "" {
					return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Failed to read request body"}}`))}, nil
				}
				if result == "transport_failure" {
					return nil, io.ErrUnexpectedEOF
				}
				if result == "http_failure" {
					return &http.Response{StatusCode: 502, Body: http.NoBody}, nil
				}
				return okResponse("plain succeeded"), nil
			})
			if result == "transport_failure" {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			} else {
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
			}
			require.Equal(t, []string{"br", "zstd", "deflate", ""}, attempts)
			resp, err = registry.Execute(816, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
				if result == "success" {
					require.Empty(t, req.Header.Get("Content-Encoding"))
				} else {
					require.Equal(t, "br", req.Header.Get("Content-Encoding"), "failed plain attempt cannot disprove compression")
				}
				return okResponse("ok"), nil
			})
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestExecuteBodyReadIdentityRetriesAfterTTL(t *testing.T) {
	registry := NewRegistry()
	now := time.Date(2026, 11, 6, 22, 0, 0, 0, time.UTC)
	registry.nowFunc = func() time.Time { return now }
	registry.Sync(817, true, time.Time{})
	brotliRecovered := false
	var attempts []string
	send := func(req *http.Request) (*http.Response, error) {
		coding := req.Header.Get("Content-Encoding")
		attempts = append(attempts, coding)
		if coding == "" {
			return okResponse("identity"), nil
		}
		if brotliRecovered && Algorithm(coding) == AlgorithmBrotli {
			return okResponse("brotli-recovered"), nil
		}
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Failed to read request body"}}`))}, nil
	}

	resp, err := registry.Execute(817, newTestRequest(t, testBody()), send)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br", "zstd", "deflate", ""}, attempts)

	now = now.Add(ambiguousIdentityRetryTTL - time.Second)
	attempts = nil
	resp, err = registry.Execute(817, newTestRequest(t, testBody()), send)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{""}, attempts)

	now = now.Add(time.Second)
	brotliRecovered = true
	attempts = nil
	resp, err = registry.Execute(817, newTestRequest(t, testBody()), send)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br"}, attempts)
	require.Equal(t, AlgorithmBrotli, registry.SettledAlgorithm(context.Background(), 817))
}

func TestExecuteInitialMediaTypeFailureDoesNotPoisonNegotiation(t *testing.T) {
	registry := NewRegistry()
	registry.Sync(818, true, time.Time{})
	var attempts []string
	resp, err := registry.Execute(818, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		attempts = append(attempts, req.Header.Get("Content-Encoding"))
		return &http.Response{StatusCode: http.StatusUnsupportedMediaType, Body: http.NoBody}, nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, []string{"br", "zstd", "deflate", ""}, attempts)
	resp, err = registry.Execute(818, newTestRequest(t, testBody()), func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "br", req.Header.Get("Content-Encoding"), "a Content-Type failure did not disprove any coding")
		return okResponse("recovered"), nil
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
}

func TestRegistryCachesShortTimeoutRefreshClientWithoutChangingParent(t *testing.T) {
	_, client := sharedTestRedis(t)
	originalReadTimeout := client.Options().ReadTimeout
	registry := sharedTestRegistry(client)
	refresh := registry.sharedRefreshRedis()
	require.NotNil(t, refresh)
	require.NotSame(t, client, refresh)
	require.Same(t, refresh, registry.sharedRefreshRedis(), "renewals reuse the configured client clone")
	require.Equal(t, sharedRefreshTimeout, refresh.Options().ReadTimeout)
	require.Equal(t, sharedRefreshTimeout, refresh.Options().WriteTimeout)
	require.Equal(t, originalReadTimeout, client.Options().ReadTimeout)
	registry.SetRedis(nil)
	require.Nil(t, registry.sharedRedis())
	require.Nil(t, registry.sharedRefreshRedis())
}

func TestInspectEncodingResponseRestoresBodyAndReadError(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		body := "prefix"
		if oversized {
			body = strings.Repeat("x", int(maxRejectionBodyBytes)+10)
		}
		original := &replayBody{
			reader: io.MultiReader(strings.NewReader(body), &replayReadError{err: io.ErrUnexpectedEOF}),
			closer: io.NopCloser(strings.NewReader("")),
		}
		resp := &http.Response{StatusCode: 500, Body: original}
		require.Equal(t, responseFailureHTTP, inspectEncodingResponse(resp))
		got, err := io.ReadAll(resp.Body)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.Equal(t, body, string(got))
		require.NoError(t, resp.Body.Close())
	}
}
