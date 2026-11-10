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
// Initial trials can try other codings and finally identity. Once a coding has
// succeeded, a coded attempt that fails with a coding-sensitive status (see
// codingSensitiveStatuses) is replayed once without content coding, so the
// caller only sees the plain outcome: a plain success pins identity for
// ambiguousIdentityRetryTTL, a plain failure keeps the coding. Other HTTP and
// transport failures do not disprove the coding and are delivered as is. Only
// an explicit request content-coding rejection retires a settled coding.
//
// A coded attempt may be discarded, so it is measured in its own probe
// collector: only the attempt returned to the caller is adopted into the
// request's usage record, while a discarded one is logged with its wire cost.
// Probing therefore never inflates the traffic a client request reports.
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
	if negotiation == nil || req == nil {
		return send(req)
	}
	if err := r.hydrateShared(req.Context(), accountID, negotiation); err != nil {
		// Without a readable shared snapshot, probing br again could overwrite a
		// coding another instance already pinned. Send identity for this request
		// and retry shared initialization on the next one.
		slog.Warn("reqcompress shared state load failed",
			append(requestLogFields(req, accountID, AlgorithmNone, nil), "error", err)...)
		return send(req)
	}
	// Every account request renews a known coding, even when this particular
	// body is too small, signed, or otherwise ineligible for compression.
	r.refreshSharedAlgorithm(req.Context(), accountID, negotiation)
	if !Eligible(req) {
		return send(req)
	}

	work, ownsNegotiation := negotiation.begin(r.now())
	if len(work) == 0 {
		return send(req)
	}
	levels := r.compressionLevels(req.Context())
	lastStatus := 0
	unverifiedCandidates := false
	for len(work) > 0 {
		algorithm := work[0]
		work = work[1:]

		attempt, err := encodeRequestWithLevels(req, algorithm, levels)
		if err != nil {
			slog.Warn("reqcompress encode failed",
				append(requestLogFields(req, accountID, algorithm, nil), "error", err,
					"scope", "this_request_only")...)
			if ownsNegotiation {
				negotiation.abortNegotiation()
			}
			return send(req)
		}
		// This attempt may be discarded, so its bytes are measured apart from the
		// request. Adopting it on delivery keeps the usage record limited to the
		// request the client actually received.
		probe := traffic.NewCollector()
		attempt = attempt.WithContext(traffic.WithCollector(attempt.Context(), probe))
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
			// trial open so the next request probes again. The attempt never
			// delivered a response, but its sent bytes are the request's own.
			adoptDeliveredAttempt(req, probe)
			if ownsNegotiation {
				negotiation.abortNegotiation()
			}
			logPreservedCoding(req, accountID, algorithm, negotiation, resp, "transport_error", err)
			return resp, err
		}
		failureKind := inspectEncodingResponse(resp)
		// A bare 415 may reject the media type, not the content coding. It can
		// guide an unverified trial, but cannot invalidate a proven coding.
		rejected := failureKind == responseFailureContentCoding ||
			(ownsNegotiation && resp != nil && resp.StatusCode == http.StatusUnsupportedMediaType)
		if !rejected {
			if encodingAccepted(resp) {
				adoptDeliveredAttempt(req, probe)
				if ownsNegotiation {
					negotiation.negotiationSucceeded(algorithm)
					r.persistSharedAlgorithm(req.Context(), accountID, algorithm)
					fields := append(requestLogFields(req, accountID, algorithm, resp),
						"original_bytes", originalBytes, "compressed_bytes", attemptedBytes)
					if hasOriginalBytes && originalBytes > 0 {
						fields = append(fields, "ratio_percent", (1-float64(attemptedBytes)/float64(originalBytes))*100)
					}
					slog.Info("reqcompress coding settled", fields...)
				}
				return resp, nil
			}
			if !ownsNegotiation {
				if isCodingSensitiveFailure(resp) && req.Context().Err() == nil {
					return r.replayWithoutCoding(req, accountID, algorithm, negotiation, resp, probe, failureKind, send)
				}
				// Auth, permission, rate-limit and availability failures say
				// nothing about coding support, and a canceled caller cannot
				// use a replay. Deliver them without multiplying uploads.
				adoptDeliveredAttempt(req, probe)
				logPreservedCoding(req, accountID, algorithm, negotiation, resp, failureKind, nil)
				return resp, nil
			}
			if !shouldProbeAfterInitialFailure(resp, failureKind) {
				// An auth/rate/health error also proves nothing during the
				// initial trial. Do not upload the body four times or pin
				// identity just because the provider is temporarily unhealthy.
				negotiation.abortNegotiation()
				adoptDeliveredAttempt(req, probe)
				return resp, nil
			}
			// The coding may have caused this failure. Try every coding only in
			// an unverified trial, without permanently removing this candidate.
			unverifiedCandidates = true
			fields := append(requestLogFields(req, accountID, algorithm, resp),
				"reason", failureKind, "error_source", "upstream_response")
			slog.Warn("reqcompress coding unverified", append(fields, discardedAttemptLogFields(probe)...)...)
			drainAndClose(resp)
			continue
		}
		slog.Warn("reqcompress coding rejected",
			append(requestLogFields(req, accountID, algorithm, resp), discardedAttemptLogFields(probe)...)...)
		drainAndClose(resp)
		if ownsNegotiation {
			if failureKind == responseFailureContentCoding {
				negotiation.negotiationCandidateFailed(algorithm)
			} else {
				// A bare 415 can reject Content-Type for every representation.
				// Keep this candidate unless a plain attempt actually succeeds.
				unverifiedCandidates = true
			}
			continue
		}
		// The settled coding broke: drop it and, when this request becomes the
		// owner, try the remaining codings immediately.
		next, owns := negotiation.retireSettled(algorithm)
		r.deleteSharedAlgorithm(req.Context(), accountID, algorithm)
		if !owns {
			break
		}
		slog.Warn("reqcompress settled coding retired",
			append(requestLogFields(req, accountID, algorithm, resp), "remaining_candidates", len(next),
				"reason", responseFailureContentCoding)...)
		ownsNegotiation = true
		work = append(work, next...)
	}
	identityResp, identityErr := send(req)
	if ownsNegotiation {
		if !unverifiedCandidates || (identityErr == nil && encodingAccepted(identityResp)) {
			reason := "all_codings_rejected"
			if unverifiedCandidates {
				reason = "initial_identity_accepted"
				retryAt := r.now().Add(ambiguousIdentityRetryTTL)
				negotiation.pinIdentityUntil(retryAt)
				r.persistSharedIdentity(req.Context(), accountID, negotiation, retryAt, AlgorithmNone)
			} else {
				negotiation.pinIdentity()
			}
			fields := append(requestLogFields(req, accountID, AlgorithmNone, identityResp),
				"reason", reason, "last_status", lastStatus)
			if unverifiedCandidates {
				fields = append(fields, "identity_ttl_seconds", int(ambiguousIdentityRetryTTL/time.Second))
			}
			slog.Warn("reqcompress no coding accepted", fields...)
			slog.Warn("reqcompress pinned identity", fields...)
		} else {
			// Neither compressed nor plain attempts worked: preserve the
			// unverified candidates for a later request instead of turning
			// a transient upstream/read failure into a permanent downgrade.
			negotiation.abortNegotiation()
		}
	}
	return identityResp, identityErr
}

// replayWithoutCoding resends req without content coding after its settled
// coding produced a coding-sensitive failure, so an upstream that stopped
// decoding the coding for some entities never fails the caller. The coded
// attempt is discarded: its wire cost is logged and kept out of the usage
// record, which measures the delivered plain attempt instead. A plain success
// pins identity for ambiguousIdentityRetryTTL locally and in shared state;
// afterwards the remaining candidates, led by the failed coding, are probed
// again. A plain failure proves nothing against the coding, which stays
// settled, and is delivered in place of the coded failure.
func (r *Registry) replayWithoutCoding(req *http.Request, accountID int64, algorithm Algorithm, negotiation *state,
	coded *http.Response, probe *traffic.Collector, reason string, send SendFunc) (*http.Response, error) {
	fields := append(requestLogFields(req, accountID, algorithm, coded), discardedAttemptLogFields(probe)...)
	drainAndClose(coded)
	resp, err := send(req)
	pinned := false
	if err == nil && encodingAccepted(resp) {
		retryAt := r.now().Add(ambiguousIdentityRetryTTL)
		if pinned = negotiation.demoteSettled(algorithm, retryAt); pinned {
			r.persistSharedIdentity(req.Context(), accountID, negotiation, retryAt, algorithm)
		}
	}
	logIdentityReplay(req, negotiation, algorithm, fields, reason, resp, err, pinned)
	return resp, err
}

// encodingAccepted reports whether a response proves the upstream decoded the
// request entity. Only success settles a coding: a 401/429/5xx from a different
// failure layer would otherwise pin a coding that was never exercised.
func encodingAccepted(resp *http.Response) bool {
	return resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 400
}

// adoptDeliveredAttempt publishes the probe of the attempt being returned to the
// caller. The request's collector aggregates every attempt it is given, so an
// attempt that is delivered must be moved into it while a discarded probe stays
// out of the usage record.
func adoptDeliveredAttempt(req *http.Request, probe *traffic.Collector) {
	if req == nil || probe == nil {
		return
	}
	if target := traffic.FromContext(req.Context()); target != nil {
		target.Adopt(probe)
	}
}

// discardedAttemptLogFields reports the wire cost of an attempt that is retried
// or dropped, so probing remains observable without being charged to the
// request's usage record.
func discardedAttemptLogFields(probe *traffic.Collector) []any {
	usage := probe.Snapshot()
	if usage == nil || usage.Upstream == nil || usage.Upstream.Request == nil {
		return nil
	}
	measurement := usage.Upstream.Request
	fields := make([]any, 0, 6)
	if measurement.OriginalBytes != nil {
		fields = append(fields, "original_bytes", *measurement.OriginalBytes)
	}
	if measurement.ActualBytes != nil {
		fields = append(fields, "sent_bytes", *measurement.ActualBytes)
	}
	if ratio := measurement.Ratio(); ratio != nil {
		fields = append(fields, "ratio_percent", *ratio)
	}
	return fields
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
