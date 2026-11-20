package handler

import (
	"net/http"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"go.uber.org/zap"
)

// requestBodyReadFailure maps failures to the caller's protocol-specific response.
// The shared request body reader logs failures once with payload-free diagnostics.
func requestBodyReadFailure(_ *zap.Logger, req *http.Request, err error) (int, string) {
	failure := pkghttputil.DescribeRequestBodyReadFailure(req, err)
	return failure.Status, failure.ClientMessage()
}
