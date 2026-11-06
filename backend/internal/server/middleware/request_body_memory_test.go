package middleware

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
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

func TestRejectRequestBodyReadErrorCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
		committed bool
	}{
		{name: "cancelled", cancelled: true},
		{name: "cancelled_after_commit", cancelled: true, committed: true},
		{name: "ordinary_read_error"},
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
			require.Equal(t, tc.cancelled, RejectRequestBodyReadError(c, io.ErrUnexpectedEOF))
			require.Equal(t, tc.cancelled, c.IsAborted())
			status := http.StatusOK
			if tc.cancelled && !tc.committed {
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
			if RejectRequestBodyReadError(c, err) {
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
	require.True(t, RejectRequestBodyReadError(c, os.ErrDeadlineExceeded))
	require.Equal(t, 499, c.Writer.Status())
	require.Empty(t, recorder.Body.String())
}
