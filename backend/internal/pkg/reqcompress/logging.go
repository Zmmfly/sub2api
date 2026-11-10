package reqcompress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// logHost returns the upstream host only. Paths and query strings are omitted
// because some providers carry credentials in the URL.
func logHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return req.URL.Host
}

// requestLogFields correlates compression decisions with the ingress request
// without exposing upstream URL paths, query credentials, or request payloads.
func requestLogFields(req *http.Request, accountID int64, algorithm Algorithm, resp *http.Response) []any {
	fields := []any{"account_id", accountID, "algorithm", algorithm,
		"component", "reqcompress", "direction", "upstream",
		"phase", "request_compression", "host", logHost(req)}
	if req != nil {
		fields = append(fields, "method", req.Method)
		if id, _ := req.Context().Value(ctxkey.RequestID).(string); id != "" {
			fields = append(fields, "request_id", id)
		}
	}
	if resp != nil {
		fields = append(fields, "status", resp.StatusCode, "upstream_request_id", upstreamRequestID(resp))
	}
	return fields
}

// logPreservedCoding distinguishes a failure reported by the upstream from
// local ingress body reads and downstream response writes. No error body is
// logged: it may echo private prompts or credentials.
func logPreservedCoding(req *http.Request, accountID int64, algorithm Algorithm, negotiation *state, resp *http.Response, reason string, cause error) {
	fields := requestLogFields(req, accountID, algorithm, resp)
	source := "upstream_response"
	if reason == "transport_error" {
		source = "upstream_transport"
		fields = append(fields, "error_kind", transportErrorKind(cause))
		if req.Context().Err() != nil {
			reason = "request_cancelled"
		}
	}
	fields = append(fields, "reason", reason, "error_source", source,
		"coding_preserved", negotiation.settledAlgorithm() == algorithm,
		"identity_retry", false)
	if negotiation.claimFailureLog(time.Now()) {
		slog.WarnContext(req.Context(), "reqcompress coding preserved", fields...)
	} else {
		slog.DebugContext(req.Context(), "reqcompress coding preserved", fields...)
	}
}

// transportErrorKind keeps diagnostics useful without logging a url.Error that
// could contain upstream query credentials or a proxy password.
func transportErrorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, syscall.EPIPE), errors.Is(err, io.ErrClosedPipe), errors.Is(err, net.ErrClosed):
		return "closed_connection"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "transport"
}

// upstreamRequestID picks common request-id response headers for log
// correlation. Provider IDs win over the Cloudflare ray ID: the ChatGPT Codex
// endpoint reports its ID only in x-oai-request-id. Missing headers yield an
// empty string.
func upstreamRequestID(resp *http.Response) string {
	if resp == nil || resp.Header == nil {
		return ""
	}
	for _, key := range []string{"x-request-id", "x-oai-request-id", "request-id", "x-amzn-requestid", "cf-ray"} {
		if value := strings.TrimSpace(resp.Header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}
