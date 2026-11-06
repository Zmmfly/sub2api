//go:build unit

package handler

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
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
	require.False(t, handleRequestBodyReadError(c, err), "corrupt compression is not a timeout/cancel/capacity case")

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
	require.True(t, handleRequestBodyReadError(c, err))
	require.Equal(t, http.StatusRequestTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), "Request body upload timed out")
	require.Equal(t, "timeout", logs.All()[0].ContextMap()["error_kind"])
	require.Len(t, logs.All(), 1)
}

func TestRequestBodyReadTruncatedFallsThroughToCaller(t *testing.T) {
	c, rec, logs := newBodyReadContext(t, &handlerBodyReader{data: []byte("x"), err: io.ErrUnexpectedEOF}, "")
	_, err := readLenientJSONRequestBodyWithPrealloc(c.Request, nil)
	require.True(t, errors.Is(err, io.ErrUnexpectedEOF))
	require.False(t, handleRequestBodyReadError(c, err))
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
	require.True(t, handleRequestBodyReadError(c, err))
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
		if !handleRequestBodyReadError(c, err) {
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
