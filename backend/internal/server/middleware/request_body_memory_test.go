package middleware

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRequestBodyMemoryRejectsOverloadAndReleasesAfterHandler(t *testing.T) {
	router := gin.New()
	held := make(chan struct{})
	release := make(chan struct{})
	router.POST("/v1/responses", RequestBodyLimit(2048, pkghttputil.NewBodyMemoryBudget(1024)), func(c *gin.Context) {
		_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if RejectRequestBodyMemoryOverload(c, err) {
			return
		}
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		if c.Query("hold") == "1" {
			close(held)
			<-release
		}
		c.Status(http.StatusNoContent)
	})
	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", 700))))
		return w
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- request("/v1/responses?hold=1") }()
	releaseHeld := sync.OnceFunc(func() { close(release) })
	defer releaseHeld()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach handler")
	}
	w := request("/v1/responses")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	releaseHeld()
	select {
	case first := <-firstDone:
		require.Equal(t, http.StatusNoContent, first.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not finish")
	}
	require.Equal(t, http.StatusNoContent, request("/v1/responses").Code)
}

// Decoding releases its workspace, but the decoded request remains live while
// the response streams. Route aliases must share that charge until the handler
// returns, rather than releasing it when decoding or the first flush completes.
func TestRequestBodyMemoryCompressedStreamRetainsBudgetUntilReturn(t *testing.T) {
	body := []byte(`{"input":"` + strings.Repeat("x", 700) + `"}`)
	for _, encoding := range []string{"gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			var writer io.WriteCloser
			if encoding == "gzip" {
				writer = gzip.NewWriter(&compressed)
			} else {
				writer = brotli.NewWriter(&compressed)
			}
			_, err := writer.Write(body)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			router := gin.New()
			budget := pkghttputil.NewBodyMemoryBudget(2500)
			held := make(chan struct{})
			resume := make(chan struct{})
			releaseHeld := sync.OnceFunc(func() { close(resume) })
			defer releaseHeld()
			handle := func(c *gin.Context) {
				decoded, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
				if RejectRequestBodyMemoryOverload(c, err) {
					return
				}
				if err != nil || !bytes.Equal(decoded, body) {
					c.AbortWithStatus(http.StatusBadRequest)
					return
				}
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString("data: ready\n\n")
				c.Writer.Flush()
				if c.Query("hold") == "1" {
					close(held)
					<-resume
				}
				runtime.KeepAlive(decoded)
			}
			for _, path := range []string{"/v1/responses", "/responses"} {
				router.POST(path, RequestBodyLimit(2048, budget), handle)
			}
			request := func(path string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(compressed.Bytes()))
				req.Header.Set("Content-Encoding", encoding)
				router.ServeHTTP(w, req)
				return w
			}
			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { firstDone <- request("/v1/responses?hold=1") }()
			select {
			case <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("compressed request did not reach the streaming handler")
			}

			rejected := request("/responses")
			require.Equal(t, http.StatusServiceUnavailable, rejected.Code)
			require.Equal(t, "1", rejected.Header().Get("Retry-After"))
			releaseHeld()
			select {
			case first := <-firstDone:
				require.Equal(t, http.StatusOK, first.Code)
				require.True(t, first.Flushed)
				require.Equal(t, "data: ready\n\n", first.Body.String())
			case <-time.After(5 * time.Second):
				t.Fatal("streaming handler did not finish")
			}
			recovered := request("/responses")
			require.Equal(t, http.StatusOK, recovered.Code)
			require.True(t, recovered.Flushed)
			require.Equal(t, "data: ready\n\n", recovered.Body.String())
		})
	}
}

func TestRejectRequestBodyReadErrorCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
		committed bool
		readErr   error
		handled   bool
	}{
		{name: "cancelled", cancelled: true, handled: true},
		{name: "cancelled_after_commit", cancelled: true, committed: true, handled: true},
		{name: "ordinary_read_error"},
		// A synthetic timeout does not override a pre-existing cancellation.
		// Real socket deadlines are covered by the HTTP/1 integration test.
		{name: "read_timeout_with_cancelled_context", cancelled: true, handled: true, readErr: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			if tc.committed {
				_, err := c.Writer.WriteString("already committed")
				require.NoError(t, err)
			}
			bodyBefore := recorder.Body.String()
			readErr := tc.readErr
			if readErr == nil {
				readErr = io.ErrUnexpectedEOF
			}
			require.Equal(t, tc.handled, RejectRequestBodyReadError(c, "test", readErr))
			require.Equal(t, tc.handled, c.IsAborted())
			status := http.StatusOK
			if tc.handled && !tc.committed {
				status = 499
			}
			require.Equal(t, status, c.Writer.Status())
			require.Equal(t, bodyBefore, recorder.Body.String())
		})
	}
}

type bodyErrReader struct {
	data []byte
	err  error
}

func (r *bodyErrReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

// readBodyThroughMiddleware drives the real ingress path: RequestBodyLimit,
// the shared reader and RejectRequestBodyReadError, as the allowlist and
// composite middlewares do.
func readBodyThroughMiddleware(t *testing.T, path string, body io.Reader, encoding string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestBodyLimit(1<<20, pkghttputil.NewBodyMemoryBudget(1<<20)))
	r.POST("/*any", func(c *gin.Context) {
		if _, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request); err != nil {
			if RejectRequestBodyReadError(c, "test", err) {
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Failed to read request body"}})
			return
		}
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, path, body)
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequestBodyReadTimeoutMapsTo408InProtocolShape(t *testing.T) {
	timeout := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	for _, tc := range []struct {
		path, contains, forbidden string
	}{
		{"/v1/responses", `"type":"invalid_request_error"`, `model_not_found`},
		{"/v1/messages", `"type":"error"`, `model_not_found`},
		{"/v1beta/models/x:generateContent", `"code":408`, `model_not_found`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := readBodyThroughMiddleware(t, tc.path, &bodyErrReader{data: []byte("partial"), err: timeout}, "")
			require.Equal(t, http.StatusRequestTimeout, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "Request body upload timed out")
			require.Contains(t, w.Body.String(), tc.contains)
			require.NotContains(t, w.Body.String(), tc.forbidden)
		})
	}
}

func TestRequestBodyReadTruncatedAndCorruptStay400(t *testing.T) {
	w := readBodyThroughMiddleware(t, "/v1/responses", &bodyErrReader{data: []byte("partial"), err: io.ErrUnexpectedEOF}, "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "Failed to read request body")

	w = readBodyThroughMiddleware(t, "/v1/responses", strings.NewReader("not gzip"), "gzip")
	require.Equal(t, http.StatusBadRequest, w.Code)

	w = readBodyThroughMiddleware(t, "/v1/responses", strings.NewReader("x"), "compress")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRequestBodyReadTimeoutDoesNotOverrideCancellation(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	require.True(t, RejectRequestBodyReadError(c, "test", os.ErrDeadlineExceeded))
	require.Equal(t, 499, c.Writer.Status())
	require.Empty(t, recorder.Body.String())
}

func TestRequestBodyReadDeadlineHTTP1Returns408(t *testing.T) {
	for _, tc := range []struct{ path, shape string }{
		{"/v1/responses", `"type":"invalid_request_error"`},
		{"/v1/messages", `"type":"error"`},
		{"/v1beta/models/x:generateContent", `"code":408`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			type readResult struct{ err, contextErr error }
			done := make(chan readResult, 1)
			router := gin.New()
			router.POST("/*any", func(c *gin.Context) {
				c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
				_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
				if !RejectRequestBodyReadError(c, "test", err) {
					c.Status(http.StatusInternalServerError)
				}
				done <- readResult{err, c.Request.Context().Err()}
			})
			server := httptest.NewServer(pkghttputil.WithWriteTimeout(
				pkghttputil.WithBodyReadTimeout(router, 50*time.Millisecond), time.Second))
			defer server.Close()
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
			_, err = io.WriteString(conn, "POST "+tc.path+" HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\nx")
			require.NoError(t, err)
			// Keep the client connected: assert the response actually received on
			// the wire, not only Gin's internal status after context cancellation.
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
			require.Contains(t, string(body), "Request body upload timed out")
			require.Contains(t, string(body), tc.shape)
			select {
			case got := <-done:
				require.ErrorIs(t, got.contextErr, context.Canceled)
				require.True(t, pkghttputil.IsRequestBodyReadDeadline(got.err))
				require.True(t, pkghttputil.IsRequestBodyReadTimeout(got.err))
			case <-time.After(time.Second):
				t.Fatal("upload deadline did not release handler")
			}
			entries := logs.All()
			require.Len(t, entries, 1)
			require.Equal(t, zapcore.WarnLevel, entries[0].Level)
			require.Equal(t, "timeout", entries[0].ContextMap()["error_kind"])
			require.Equal(t, "downstream_ingress", entries[0].ContextMap()["direction"])
		})
	}
}

func TestRequestBodyReadHTTP1ClientDisconnectRemains499(t *testing.T) {
	type readResult struct {
		err          error
		status, size int
	}
	done := make(chan readResult, 1)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		RejectRequestBodyReadError(c, "test", err)
		done <- readResult{err, c.Writer.Status(), c.Writer.Size()}
	})
	server := httptest.NewServer(pkghttputil.WithWriteTimeout(
		pkghttputil.WithBodyReadTimeout(router, time.Second), time.Second))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "POST /v1/responses HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\nx")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.Canceled)
		require.False(t, pkghttputil.IsRequestBodyReadDeadline(got.err))
		require.Equal(t, 499, got.status)
		require.Equal(t, -1, got.size, "disconnect must not write a response body")
	case <-time.After(3 * time.Second):
		t.Fatal("client disconnect did not release handler")
	}
}

func TestRequestBodyReadTimeoutDoesNotAppendToCommittedResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, err := c.Writer.WriteString("already streamed")
	require.NoError(t, err)
	require.True(t, RejectRequestBodyReadError(c, "test", os.ErrDeadlineExceeded))
	require.True(t, c.IsAborted())
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "already streamed", recorder.Body.String())
}

func TestRequestBodyReadDeadlineHTTP2Returns408WithoutClosingConnection(t *testing.T) {
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if !RejectRequestBodyReadError(c, "test", err) {
			c.Status(http.StatusInternalServerError)
		}
	})
	router.GET("/healthy", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	server := httptest.NewUnstartedServer(pkghttputil.WithWriteTimeout(
		pkghttputil.WithBodyReadTimeout(router, 100*time.Millisecond), time.Second))
	var connections atomic.Int32
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", reader)
	require.NoError(t, err)
	go func() { _, _ = writer.Write([]byte("x")) }()
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 2, resp.ProtoMajor)
	require.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
	require.Contains(t, string(body), "Request body upload timed out")
	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/healthy", nil)
	require.NoError(t, err)
	resp2, err := server.Client().Do(req2)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, 2, resp2.ProtoMajor)
	require.Equal(t, http.StatusNoContent, resp2.StatusCode)
	require.EqualValues(t, 1, connections.Load(), "healthy stream must reuse the existing HTTP/2 connection")
}
