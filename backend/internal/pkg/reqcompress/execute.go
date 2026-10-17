package reqcompress

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// SendFunc performs one upstream attempt. Implementations must not mutate the
// request: every attempt is treated as an independent measured request.
type SendFunc func(*http.Request) (*http.Response, error)

// Execute forwards req with the content coding negotiated for accountID.
//
// When the account is not opted in, the request is not eligible, or another
// request already owns the trial, req is sent unchanged. Otherwise codings are
// attempted in preference order and the first successful response is returned.
// A coded attempt that fails with an ambiguous upstream error is retried once as
// identity, so enabling compression can never turn a request that would
// otherwise succeed into a failure. Recognized encoding rejections continue to
// probe the remaining codings and end at identity as well.
func Execute(accountID int64, req *http.Request, send SendFunc) (*http.Response, error) {
	return defaultRegistry.Execute(accountID, req, send)
}

// Execute forwards req with the content coding negotiated in this registry.
func (r *Registry) Execute(accountID int64, req *http.Request, send SendFunc) (*http.Response, error) {
	if send == nil {
		return nil, errors.New("reqcompress: nil send function")
	}
	slot := r.existingSlot(accountID)
	if slot == nil {
		return send(req)
	}
	negotiation := slot.currentState()
	if negotiation == nil || !Eligible(req) {
		return send(req)
	}

	work, ownsNegotiation := negotiation.begin()
	if len(work) == 0 {
		return send(req)
	}
	lastStatus := 0
	for len(work) > 0 {
		algorithm := work[0]
		work = work[1:]

		attempt, err := encodeRequest(req, algorithm)
		if err != nil {
			slog.Warn("reqcompress encode failed", "account_id", accountID,
				"algorithm", algorithm, "host", logHost(req), "method", req.Method, "error", err)
			if ownsNegotiation {
				negotiation.abortNegotiation()
			}
			return send(req)
		}
		attemptedBytes := attempt.ContentLength
		originalBytes, hasOriginalBytes := traffic.OriginalRequestBodyBytes(attempt.Context())
		if hasOriginalBytes && originalBytes > 0 {
			slog.Debug("reqcompress attempt", "account_id", accountID,
				"algorithm", algorithm,
				"content_encoding", attempt.Header.Get("Content-Encoding"),
				"host", logHost(req), "method", req.Method,
				"original_bytes", originalBytes,
				"compressed_bytes", attemptedBytes,
				"ratio_percent", (1-float64(attemptedBytes)/float64(originalBytes))*100)
		} else {
			slog.Debug("reqcompress attempt", "account_id", accountID,
				"algorithm", algorithm,
				"content_encoding", attempt.Header.Get("Content-Encoding"),
				"host", logHost(req), "method", req.Method,
				"compressed_bytes", attemptedBytes)
		}
		resp, err := send(attempt)
		if resp != nil {
			lastStatus = resp.StatusCode
		}
		if err != nil {
			// A transport failure says nothing about the coding. Leave the
			// trial open so the next request probes again.
			if ownsNegotiation {
				negotiation.abortNegotiation()
			}
			return resp, err
		}
		if !isEncodingRejection(resp) {
			if encodingAccepted(resp) {
				if ownsNegotiation {
					negotiation.negotiationSucceeded(algorithm)
					fields := []any{"account_id", accountID, "algorithm", algorithm,
						"host", logHost(req), "method", req.Method,
						"original_bytes", originalBytes, "compressed_bytes", attemptedBytes}
					if hasOriginalBytes && originalBytes > 0 {
						fields = append(fields, "ratio_percent", (1-float64(attemptedBytes)/float64(originalBytes))*100)
					}
					slog.Info("reqcompress coding settled", fields...)
				}
				return resp, nil
			}
			if !shouldRetryIdentity(resp) {
				if ownsNegotiation {
					negotiation.abortNegotiation()
				}
				return resp, nil
			}
			// The coded attempt failed with an error that may or may not be
			// coding-related. Retry once as identity so the feature cannot turn a
			// request that would otherwise succeed into a failure.
			status := resp.StatusCode
			fallbackFields := []any{"account_id", accountID, "algorithm", algorithm,
				"status", status, "host", logHost(req), "method", req.Method,
				"upstream_request_id", upstreamRequestID(resp)}
			if negotiation.claimFallbackLog(time.Now()) {
				slog.Warn("reqcompress identity fallback", fallbackFields...)
			} else {
				slog.Debug("reqcompress identity fallback", fallbackFields...)
			}
			drainAndClose(resp)
			identityResp, identityErr := send(req)
			if identityErr != nil {
				if ownsNegotiation {
					// A failed trial must not keep probing on every request.
					negotiation.pinIdentity()
					slog.Warn("reqcompress pinned identity", "account_id", accountID,
						"algorithm", algorithm, "status", status, "host", logHost(req),
						"method", req.Method, "reason", "fallback_error", "error", identityErr)
				}
				return identityResp, identityErr
			}
			if ownsNegotiation || encodingAccepted(identityResp) {
				// A successful identity attempt proves the coding caused the
				// failure; a failed one during the trial still must not keep
				// re-probing on every request.
				reason := "fallback_succeeded"
				if !encodingAccepted(identityResp) {
					reason = "trial_failed"
				}
				negotiation.pinIdentity()
				slog.Warn("reqcompress pinned identity", "account_id", accountID,
					"algorithm", algorithm, "status", status, "host", logHost(req),
					"method", req.Method, "reason", reason)
			}
			return identityResp, nil
		}
		status := resp.StatusCode
		slog.Warn("reqcompress coding rejected", "account_id", accountID,
			"algorithm", algorithm, "status", status, "host", logHost(req),
			"method", req.Method, "upstream_request_id", upstreamRequestID(resp))
		drainAndClose(resp)
		if ownsNegotiation {
			negotiation.negotiationCandidateFailed(algorithm)
			continue
		}
		// The settled coding broke: drop it and, when this request becomes the
		// owner, try the remaining codings immediately.
		next, owns := negotiation.retireSettled(algorithm)
		if !owns {
			break
		}
		slog.Warn("reqcompress settled coding retired", "account_id", accountID,
			"algorithm", algorithm, "status", status, "host", logHost(req),
			"method", req.Method, "remaining_candidates", len(next))
		ownsNegotiation = true
		work = append(work, next...)
	}
	if ownsNegotiation {
		slog.Warn("reqcompress no coding accepted", "account_id", accountID,
			"host", logHost(req), "method", req.Method, "last_status", lastStatus)
	}
	return send(req)
}

// encodingAccepted reports whether a response proves the upstream decoded the
// request entity. Only success settles a coding: a 401/429/5xx from a different
// failure layer would otherwise pin a coding that was never exercised.
func encodingAccepted(resp *http.Response) bool {
	return resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 400
}

// drainAndClose releases a rejected attempt while keeping the connection
// reusable when the remaining error body is small.
func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRejectionBodyBytes))
	_ = resp.Body.Close()
}
