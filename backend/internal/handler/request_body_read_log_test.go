//go:build unit

package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type handlerBodyReader struct {
	data []byte
	err  error
}

func (r *handlerBodyReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func newBodyReadContext(t *testing.T, body io.Reader, encoding string) (*gin.Context, *httptest.ResponseRecorder, *observer.ObservedLogs) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "rid-1")))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses?token=secret-payload-marker", body).WithContext(ctx)
	if encoding != "" {
		c.Request.Header.Set("Content-Encoding", encoding)
	}
	return c, rec, logs
}

// The handler read path logs once via the shared reader (no per-handler log).
func TestLogRequestBodyReadFailureOnceThroughHandlerReader(t *testing.T) {
	c, _, logs := newBodyReadContext(t, strings.NewReader("secret-payload-marker"), "gzip")
	_, err := readLenientJSONRequestBodyWithPrealloc(c.Request, nil)
	require.Error(t, err)
	require.False(t, handleRequestBodyReadError(c, nil, err), "corrupt compression is not a timeout/cancel/capacity case")
	status, message := requestBodyReadFailure(nil, c.Request, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "Failed to read request body", message)

	entries := logs.All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "downstream_ingress", fields["direction"])
	require.Equal(t, "read_request_body", fields["phase"])
	require.Equal(t, "decode_content_encoding", fields["error_kind"])
	require.Equal(t, "gzip", fields["content_encoding"])
	require.Equal(t, "rid-1", fields["request_id"])
	require.NotContains(t, entries[0].Message+fmt.Sprint(fields), "secret-payload-marker")
}

func TestRequestBodyReadTimeoutReturns408FromHandlerHelper(t *testing.T) {
	c, rec, logs := newBodyReadContext(t, &handlerBodyReader{data: []byte("x"), err: &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}}, "")
	_, err := readLenientJSONRequestBodyWithPrealloc(c.Request, nil)
	require.Error(t, err)
	require.True(t, handleRequestBodyReadError(c, nil, err))
	require.Equal(t, http.StatusRequestTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "Request body upload timed out")
	require.Equal(t, "timeout", logs.All()[0].ContextMap()["error_kind"])
	require.Len(t, logs.All(), 1)
}

func TestRequestBodyReadTruncatedFallsThroughToCaller(t *testing.T) {
	c, rec, logs := newBodyReadContext(t, &handlerBodyReader{data: []byte("x"), err: io.ErrUnexpectedEOF}, "")
	_, err := readLenientJSONRequestBodyWithPrealloc(c.Request, nil)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))
	require.False(t, handleRequestBodyReadError(c, nil, err))
	require.Empty(t, rec.Body.String())
	require.Equal(t, "truncated_body", logs.All()[0].ContextMap()["error_kind"])
}

func TestRequestBodyReadCancelledWritesNoBody(t *testing.T) {
	c, rec, logs := newBodyReadContext(t, strings.NewReader("x"), "")
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	_, err := readLenientJSONRequestBodyWithPrealloc(c.Request, nil)
	require.Error(t, err)
	require.True(t, handleRequestBodyReadError(c, nil, err))
	require.Equal(t, 499, c.Writer.Status())
	require.Empty(t, rec.Body.String())
	require.Len(t, logs.All(), 1)
	require.Equal(t, "client_disconnect", logs.All()[0].ContextMap()["error_kind"])
}

func TestRequestBodyReadHTTP1DeadlineReturns408FromHandlerHelper(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
		_, err := readLenientJSONRequestBodyWithPrealloc(c.Request, nil)
		if !handleRequestBodyReadError(c, nil, err) {
			c.Status(http.StatusInternalServerError)
		}
	})
	server := httptest.NewServer(pkghttputil.WithWriteTimeout(
		pkghttputil.WithBodyReadTimeout(router, 50*time.Millisecond), time.Second))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
	_, err = io.WriteString(conn, "POST /v1/responses HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\nx")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
	require.Contains(t, string(body), "Request body upload timed out")
	entries := logs.All()
	require.Len(t, entries, 1)
	require.Equal(t, zapcore.WarnLevel, entries[0].Level)
	require.Equal(t, "timeout", entries[0].ContextMap()["error_kind"])
}

func TestRequestBodyReadFailureKeepsBodyLimitMessageWithoutLogging(t *testing.T) {
	log, logs := newObservedLogger(t)
	status, message := requestBodyReadFailure(log, nil, &http.MaxBytesError{Limit: 2 << 20})
	require.Equal(t, http.StatusRequestEntityTooLarge, status)
	require.Equal(t, buildBodyTooLargeMessage(2<<20), message)
	require.Empty(t, logs.All())
}

// cancelOnReadErr models net/http on HTTP/1: a failed body read cancels the
// request context before the error reaches the handler.
type cancelOnReadErr struct {
	err    error
	cancel context.CancelFunc
}

func (r cancelOnReadErr) Read([]byte) (int, error) {
	r.cancel()
	return 0, r.err
}

// The Anthropic /v1/messages handler reads the full body before it knows the
// model or account. A failed read must report why instead of a generic 400.
func TestGatewayMessagesClassifiesRequestBodyReadFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deadlineErr := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}

	tests := []struct {
		name        string
		readErr     error
		cancelCtx   bool
		wantStatus  int
		wantMessage string
	}{
		{"h2 read timeout", deadlineErr, false, http.StatusRequestTimeout, "Request body upload timed out"},
		// A timeout without our installed deadline marker does not override
		// cancellation. The real HTTP/1 deadline is covered above.
		{"cancelled context with unmarked timeout", deadlineErr, true, 499, ""},
		// A client that went away gets no body, matching the failover path.
		{"client disconnect", io.ErrUnexpectedEOF, true, 499, ""},
		{"truncated body", io.ErrUnexpectedEOF, false, http.StatusBadRequest, "Failed to read request body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failing := iotest.ErrReader(tc.readErr)
			if tc.cancelCtx {
				failing = cancelOnReadErr{err: tc.readErr, cancel: cancel}
			}
			body := io.MultiReader(strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[`), failing)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", body).WithContext(ctx)
			req.ContentLength = 1 << 20
			c.Request = req
			groupID := int64(1)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 1, GroupID: &groupID})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})

			(&GatewayHandler{}).Messages(c)

			require.Equal(t, tc.wantStatus, c.Writer.Status())
			if tc.wantMessage == "" {
				require.Empty(t, rec.Body.String())
				return
			}
			var resp struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, "error", resp.Type)
			require.Equal(t, "invalid_request_error", resp.Error.Type)
			require.Equal(t, tc.wantMessage, resp.Error.Message)
		})
	}
}

// Google-format errors carry no type field; body read failures must still be
// attributed to the client request rather than the platform.
func TestGeminiRequestBodyReadTimeoutClassifiedAsClient(t *testing.T) {
	entry := recordGoogleFormatOpsError(t, func(c *gin.Context) {
		c.Request.Body = io.NopCloser(iotest.ErrReader(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}))
		readGeminiCachedContentBody(c)
	})
	require.Equal(t, http.StatusRequestTimeout, entry.StatusCode)
	require.Equal(t, "invalid_request_error", entry.ErrorType)
	require.Equal(t, "request", entry.ErrorPhase)
	require.Equal(t, "client", entry.ErrorOwner)
	require.Equal(t, "P3", entry.Severity)
}
