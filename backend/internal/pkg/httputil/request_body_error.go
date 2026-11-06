package httputil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"go.uber.org/zap"
)

// ErrUnsupportedContentEncoding marks a request Content-Encoding the gateway
// cannot decode. It is always wrapped in a *ContentEncodingError.
var ErrUnsupportedContentEncoding = errors.New("unsupported Content-Encoding")

// ContentEncodingError reports that decoding a request body's Content-Encoding
// failed. Err keeps the underlying cause (corrupt stream, unsupported coding,
// size limit, memory budget...) reachable through errors.Is/As.
type ContentEncodingError struct {
	Encoding string
	Err      error
}

func (e *ContentEncodingError) Error() string {
	return fmt.Sprintf("decode Content-Encoding %q: %v", e.Encoding, e.Err)
}

func (e *ContentEncodingError) Unwrap() error { return e.Err }

// Request body read error kinds. They are bounded, payload-free labels used in
// logs; they are not shown to clients.
const (
	RequestBodyErrorKindMaxBytes                   = "max_bytes"
	RequestBodyErrorKindCapacity                   = "capacity"
	RequestBodyErrorKindDecodeContentEncoding      = "decode_content_encoding"
	RequestBodyErrorKindUnsupportedContentEncoding = "unsupported_content_encoding"
	RequestBodyErrorKindClientDisconnect           = "client_disconnect"
	RequestBodyErrorKindTimeout                    = "timeout"
	RequestBodyErrorKindTruncatedBody              = "truncated_body"
	RequestBodyErrorKindTransport                  = "transport"
	RequestBodyErrorKindIORead                     = "io_read"
)

// RequestBodyReadErrorKind classifies an error returned by the request body
// readers. Typed errors are preferred; a string match remains only as a
// compatibility fallback for errors created outside this package.
func RequestBodyReadErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr):
		return RequestBodyErrorKindMaxBytes
	case errors.Is(err, ErrRequestBodyMemoryBudget):
		return RequestBodyErrorKindCapacity
	}
	var encErr *ContentEncodingError
	if errors.As(err, &encErr) {
		if errors.Is(err, ErrUnsupportedContentEncoding) {
			return RequestBodyErrorKindUnsupportedContentEncoding
		}
		return RequestBodyErrorKindDecodeContentEncoding
	}
	if lower := strings.ToLower(err.Error()); strings.Contains(lower, "decode content-encoding") {
		if strings.Contains(lower, "unsupported content-encoding") {
			return RequestBodyErrorKindUnsupportedContentEncoding
		}
		return RequestBodyErrorKindDecodeContentEncoding
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed) {
		return RequestBodyErrorKindClientDisconnect
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return RequestBodyErrorKindTimeout
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return RequestBodyErrorKindTruncatedBody
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return RequestBodyErrorKindTimeout
		}
		return RequestBodyErrorKindTransport
	}
	return RequestBodyErrorKindIORead
}

// IsRequestBodyReadTimeout reports whether err is a stalled-upload timeout
// (a per-read socket deadline), as opposed to a decode or size failure.
func IsRequestBodyReadTimeout(err error) bool {
	return RequestBodyReadErrorKind(err) == RequestBodyErrorKindTimeout
}

// ContentEncodingCategory maps request Content-Encoding headers onto a fixed
// label set so arbitrary client-supplied values never reach logs.
func ContentEncodingCategory(header http.Header) string {
	if header == nil {
		return "identity"
	}
	switch strings.ToLower(strings.TrimSpace(traffic.ContentEncoding(header))) {
	case "", "identity":
		return "identity"
	case "gzip", "x-gzip":
		return "gzip"
	case "zstd":
		return "zstd"
	case "deflate":
		return "deflate"
	case "br":
		return "br"
	default:
		return "other"
	}
}

// logRequestBodyReadFailure is the single place ingress body read failures are
// logged: every reader funnels through readRequestBody / the lenient wrapper,
// so each failure is recorded exactly once regardless of the calling handler
// or middleware. It records bounded labels only, never the error text, body,
// query string, or credentials.
func logRequestBodyReadFailure(req *http.Request, err error) {
	if req == nil || err == nil {
		return
	}
	ctx := req.Context()
	kind := RequestBodyReadErrorKind(err)
	fields := []zap.Field{
		zap.String("direction", "downstream_ingress"),
		zap.String("phase", "read_request_body"),
		zap.String("error_kind", kind),
		zap.String("content_encoding", ContentEncodingCategory(req.Header)),
		zap.Int64("content_length", req.ContentLength),
	}
	log := logger.FromContext(ctx)
	if kind == RequestBodyErrorKindClientDisconnect || ctx.Err() != nil {
		log.Info("read request body failed", fields...)
		return
	}
	log.Warn("read request body failed", fields...)
}
