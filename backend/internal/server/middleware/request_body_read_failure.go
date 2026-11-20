package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
)

// AbortWithRequestBodyReadFailure writes the shared response for a failed body
// read in middleware that inspects the body before the handler. It applies the
// same classification, client message and protocol-shaped response as gateway
// handlers; the shared reader logs payload-free diagnostics once.
func AbortWithRequestBodyReadFailure(c *gin.Context, _ string, err error) {
	failure := httputil.DescribeRequestBodyReadFailure(c.Request, err)
	writeGatewayInvalidRequest(c, failure.Status, failure.ClientMessage())
	c.Abort()
}
