package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
)

// AbortWithRequestBodyReadFailure writes the shared response for a failed body
// read in middleware that inspects the body before the handler. It applies the
// same classification as gateway handlers; the shared reader logs diagnostics.
func AbortWithRequestBodyReadFailure(c *gin.Context, _ string, err error) {
	failure := httputil.DescribeRequestBodyReadFailure(c.Request, err)
	c.AbortWithStatusJSON(failure.Status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": failure.Message}})
}
