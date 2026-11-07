package middleware

import (
	"errors"
	"net/http"
	"strings"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RejectRequestBodyReadError handles early ingress termination before the route
// handler runs. Cancelled clients have no response body and an uncommitted
// response is marked 499, matching the gateway handler's cancellation path.
// Only a deadline observed by our upload reader overrides cancellation.
// The shared reader owns failure logging, including progress diagnostics.
func RejectRequestBodyReadError(c *gin.Context, _ string, err error) bool {
	if err != nil && c != nil && c.Request != nil && c.Request.Context().Err() != nil && !pkghttputil.IsRequestBodyReadDeadline(err) {
		if !c.Writer.Written() {
			c.Status(499)
		}
		c.Abort()
		return true
	}
	if RejectRequestBodyMemoryOverload(c, err) {
		return true
	}
	return RejectRequestBodyReadTimeout(c, err)
}

// RejectRequestBodyReadTimeout maps a stalled upload (per-read socket deadline)
// to 408 in the protocol's error shape, instead of the generic 400 used for
// malformed bodies. It does not touch cancelled-client, size or capacity errors.
func RejectRequestBodyReadTimeout(c *gin.Context, err error) bool {
	if c == nil || !pkghttputil.IsRequestBodyReadTimeout(err) {
		return false
	}
	if c.Writer.Written() {
		// Never append an error document to an already committed stream.
		c.Abort()
		return true
	}
	const message = "Request body upload timed out"
	path := ""
	if c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	switch {
	case strings.HasPrefix(path, "/v1beta") || strings.HasPrefix(path, "/antigravity/v1beta"):
		service.SetOpsLocalErrorType(c, "invalid_request_error", "")
		GoogleErrorWriter(c, http.StatusRequestTimeout, message)
	case strings.Contains(path, "/messages"):
		c.JSON(http.StatusRequestTimeout, gin.H{
			"type":  "error",
			"error": gin.H{"type": "invalid_request_error", "message": message},
		})
	default:
		c.JSON(http.StatusRequestTimeout, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
	}
	c.Abort()
	return true
}

// RejectRequestBodyMemoryOverload keeps temporary capacity errors distinct
// from invalid JSON and the per-request 413 size limit.
func RejectRequestBodyMemoryOverload(c *gin.Context, err error) bool {
	if !errors.Is(err, pkghttputil.ErrRequestBodyMemoryBudget) {
		return false
	}
	c.Header("Retry-After", "1")
	groupModelAllowlistErrorWriter(c)(c, http.StatusServiceUnavailable, "Request body buffering capacity is exhausted; retry later")
	c.Abort()
	return true
}
