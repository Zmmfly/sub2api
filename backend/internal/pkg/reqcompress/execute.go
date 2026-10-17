package reqcompress

import (
	"errors"
	"io"
	"net/http"
)

// SendFunc performs one upstream attempt. Implementations must not mutate the
// request: every attempt is treated as an independent measured request.
type SendFunc func(*http.Request) (*http.Response, error)

// Execute forwards req with the content coding negotiated for accountID.
//
// When the account is not opted in, the request is not eligible, or another
// request already owns the trial, req is sent unchanged. Otherwise codings are
// attempted in preference order and the first response that is not an
// encoding rejection is returned. If every coding is rejected, the request is
// forwarded as identity so the caller still observes the authoritative answer.
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
	for len(work) > 0 {
		algorithm := work[0]
		work = work[1:]

		attempt, err := encodeRequest(req, algorithm)
		if err != nil {
			if ownsNegotiation {
				negotiation.abortNegotiation()
			}
			return send(req)
		}
		resp, err := send(attempt)
		if err != nil {
			// A transport failure says nothing about the coding. Leave the
			// trial open so the next request probes again.
			if ownsNegotiation {
				negotiation.abortNegotiation()
			}
			return resp, err
		}
		if !isEncodingRejection(resp) {
			if ownsNegotiation {
				if encodingAccepted(resp) {
					negotiation.negotiationSucceeded(algorithm)
				} else {
					// An error that proves nothing about the coding (auth,
					// rate limit, upstream outage) must not pin it for the
					// process lifetime. Return the authoritative response and
					// keep the trial open for the next request.
					negotiation.abortNegotiation()
				}
			}
			return resp, nil
		}
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
		ownsNegotiation = true
		work = append(work, next...)
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
