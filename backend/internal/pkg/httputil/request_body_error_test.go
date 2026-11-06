package httputil

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// docs/REQUEST_COMPRESSION.md: ingress failure categories and payload-free logs.
const bodyErrSecret = "secret-payload-marker"

func observedRequest(t *testing.T, body io.Reader, encoding string) (*http.Request, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "rid-1")))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses?api_key="+bodyErrSecret, body).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+bodyErrSecret)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	return req, logs
}

func requireOneIngressLog(t *testing.T, logs *observer.ObservedLogs, kind, encoding string) map[string]any {
	t.Helper()
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 log entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["direction"] != "downstream_ingress" || fields["phase"] != "read_request_body" ||
		fields["error_kind"] != kind || fields["content_encoding"] != encoding || fields["request_id"] != "rid-1" {
		t.Fatalf("unexpected fields: %v", fields)
	}
	if _, ok := fields["content_length"]; !ok {
		t.Fatalf("missing content_length: %v", fields)
	}
	for _, k := range []string{"error", "err", "path", "query"} {
		if _, ok := fields[k]; ok {
			t.Fatalf("unexpected field %q", k)
		}
	}
	if s := entries[0].Message + fmt.Sprint(fields); strings.Contains(s, bodyErrSecret) {
		t.Fatalf("log leaked payload/credential: %s", s)
	}
	return fields
}

type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestRequestBodyReadErrorKind(t *testing.T) {
	timeoutErr := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"max", &http.MaxBytesError{Limit: 1}, "max_bytes"},
		{"capacity", ErrRequestBodyMemoryBudget, "capacity"},
		{"capacity_in_decode", &ContentEncodingError{Encoding: "gzip", Err: ErrRequestBodyMemoryBudget}, "capacity"},
		{"max_in_decode", &ContentEncodingError{Encoding: "gzip", Err: &http.MaxBytesError{Limit: 1}}, "max_bytes"},
		{"decode", &ContentEncodingError{Encoding: "gzip", Err: io.ErrUnexpectedEOF}, "decode_content_encoding"},
		{"unsupported", &ContentEncodingError{Encoding: "x", Err: ErrUnsupportedContentEncoding}, "unsupported_content_encoding"},
		{"legacy_decode", errors.New(`decode Content-Encoding "gzip": bad`), "decode_content_encoding"},
		{"legacy_unsupported", errors.New(`decode Content-Encoding "x": unsupported Content-Encoding`), "unsupported_content_encoding"},
		{"canceled", context.Canceled, "client_disconnect"},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, "client_disconnect"},
		{"timeout", timeoutErr, "timeout"},
		{"truncated", io.ErrUnexpectedEOF, "truncated_body"},
		{"other", errors.New("boom"), "io_read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestBodyReadErrorKind(tc.err); got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
	if !IsRequestBodyReadTimeout(timeoutErr) || IsRequestBodyReadTimeout(io.ErrUnexpectedEOF) {
		t.Fatal("IsRequestBodyReadTimeout misclassified")
	}
}

func TestRequestBodyReadContentEncodingCategoryIsWhitelisted(t *testing.T) {
	h := http.Header{}
	if ContentEncodingCategory(h) != "identity" {
		t.Fatal("empty should be identity")
	}
	h.Set("Content-Encoding", " GZip ")
	if ContentEncodingCategory(h) != "gzip" {
		t.Fatal("gzip")
	}
	h.Set("Content-Encoding", bodyErrSecret)
	if ContentEncodingCategory(h) != "other" {
		t.Fatal("unknown values must collapse to other")
	}
	h.Set("Content-Encoding", "identity")
	h.Add("Content-Encoding", "gzip")
	if ContentEncodingCategory(h) != "other" {
		t.Fatal("stacked values must collapse to other")
	}
}

func TestRequestBodyReadLogsCorruptEncodingOnce(t *testing.T) {
	req, logs := observedRequest(t, strings.NewReader(bodyErrSecret), "gzip")
	_, err := ReadRequestBodyWithPrealloc(req)
	var encErr *ContentEncodingError
	if !errors.As(err, &encErr) || errors.Is(err, ErrUnsupportedContentEncoding) {
		t.Fatalf("want typed decode error, got %v", err)
	}
	requireOneIngressLog(t, logs, "decode_content_encoding", "gzip")
}

func TestRequestBodyReadLogsUnsupportedEncodingOnce(t *testing.T) {
	req, logs := observedRequest(t, strings.NewReader(bodyErrSecret), bodyErrSecret)
	_, err := ReadLenientJSONRequestBodyWithPrealloc(req, 0)
	if !errors.Is(err, ErrUnsupportedContentEncoding) {
		t.Fatalf("want unsupported sentinel, got %v", err)
	}
	// The header value is attacker-controlled: only the whitelist label is logged.
	requireOneIngressLog(t, logs, "unsupported_content_encoding", "other")
}

func TestRequestBodyReadLogsTimeoutTruncatedAndDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{"timeout", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, "timeout"},
		{"truncated", io.ErrUnexpectedEOF, "truncated_body"},
		{"reset", syscall.ECONNRESET, "client_disconnect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, logs := observedRequest(t, &failingReader{data: []byte(bodyErrSecret), err: tc.err}, "")
			if _, err := ReadLenientJSONRequestBodyWithPrealloc(req, 0); !errors.Is(err, tc.err) {
				t.Fatalf("want %v, got %v", tc.err, err)
			}
			requireOneIngressLog(t, logs, tc.kind, "identity")
		})
	}
}

func TestRequestBodyReadCancelledContextLogsOnce(t *testing.T) {
	req, logs := observedRequest(t, strings.NewReader(bodyErrSecret), "")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	if _, err := ReadRequestBodyWithPrealloc(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	requireOneIngressLog(t, logs, "client_disconnect", "identity")
}

func TestRequestBodyReadMaxBytesLogsOnce(t *testing.T) {
	req, logs := observedRequest(t, strings.NewReader(strings.Repeat("a", 64)), "")
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, 8)
	if _, err := ReadRequestBodyWithPrealloc(req); err == nil {
		t.Fatal("expected max bytes error")
	}
	requireOneIngressLog(t, logs, "max_bytes", "identity")
}

func TestRequestBodyReadSuccessAndPrereadDoNotLogOrReread(t *testing.T) {
	payload := []byte(`{"model":"m"}`)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	req, logs := observedRequest(t, bytes.NewReader(gz.Bytes()), "gzip")
	got, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("decode: %q %v", got, err)
	}

	// Replay: the preread wrapper is served from memory (same slice, no decode,
	// no second socket read) and logs nothing.
	req.Body = NewPrereadBody(got)
	again, err := ReadRequestBodyWithPrealloc(req)
	if err != nil || &again[0] != &got[0] {
		t.Fatalf("preread must return the same slice without error: %v", err)
	}
	if n := len(logs.All()); n != 0 {
		t.Fatalf("success path logged %d entries", n)
	}
}

func TestRequestBodyReadFailureReleasesBudget(t *testing.T) {
	budget := NewBodyMemoryBudget(1 << 20)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	defer release()
	core, _ := observer.New(zapcore.DebugLevel)
	ctx = logger.IntoContext(ctx, zap.New(core))
	for _, tc := range []struct {
		body io.Reader
		enc  string
	}{
		{&failingReader{data: bytes.Repeat([]byte{'x'}, 4096), err: io.ErrUnexpectedEOF}, ""},
		{strings.NewReader("not gzip"), "gzip"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", tc.body).WithContext(ctx)
		if tc.enc != "" {
			req.Header.Set("Content-Encoding", tc.enc)
		}
		if _, err := ReadLenientJSONRequestBodyWithPrealloc(req, 0); err == nil {
			t.Fatal("expected failure")
		}
		budget.mu.Lock()
		used := budget.used
		budget.mu.Unlock()
		if used != 0 {
			t.Fatalf("failed read leaked %d budget bytes", used)
		}
	}
}

func TestRequestBodyReadDeadlinePreservesCauseAndReleasesBudget(t *testing.T) {
	budget := NewBodyMemoryBudget(1 << 20)
	type readResult struct {
		err, contextErr error
		used            int64
	}
	done := make(chan readResult, 1)
	server := httptest.NewServer(WithBodyReadTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release := WithRequestBodyMemoryBudget(r.Context(), budget)
		defer release()
		_, err := ReadRequestBodyWithPrealloc(r.WithContext(ctx))
		budget.mu.Lock()
		used := budget.used
		budget.mu.Unlock()
		done <- readResult{err, ctx.Err(), used}
		w.WriteHeader(http.StatusRequestTimeout)
	}), 50*time.Millisecond))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		var netErr net.Error
		if !IsRequestBodyReadDeadline(got.err) || !errors.As(got.err, &netErr) || !netErr.Timeout() {
			t.Fatalf("read lost original timeout cause: %v", got.err)
		}
		if !errors.Is(got.contextErr, context.Canceled) || errors.Is(got.err, context.Canceled) {
			t.Fatalf("read deadline must survive HTTP/1 context cancellation: err=%v context=%v", got.err, got.contextErr)
		}
		if got.used != 0 {
			t.Fatalf("timed-out read retained %d budget bytes before handler cleanup", got.used)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upload deadline did not release reader")
	}
}
