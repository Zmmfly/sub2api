package reqcompress

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxRejectionBodyBytes bounds how much of a suspicious error body is inspected
// before deciding whether the upstream rejected the content coding.
const maxRejectionBodyBytes int64 = 64 << 10

// rejectionStatuses are the statuses an upstream may use when it cannot decode
// a request entity. A settled coding requires an explicit content-coding error;
// a bare 415 may instead reject Content-Type. Other statuses (including
// 401/429/529) prove nothing about the coding.
var rejectionStatuses = map[int]struct{}{
	http.StatusBadRequest:          {},
	http.StatusUnprocessableEntity: {},
	http.StatusInternalServerError: {},
	http.StatusNotImplemented:      {},
}

// rejectionKeywords describe the failure as a content-coding problem. A status
// alone is never enough: providers also return 400 for unrelated invalid
// requests, and treating those as "coding rejected" would retry a request the
// provider may have already understood.
var rejectionKeywords = []string{
	"content-encoding",
	"content encoding",
	"unsupported encoding",
	"unsupported_encoding",
	"unsupported content coding",
	"invalid encoding",
	"invalid_encoding",
}

// Only this specific 400/422 diagnostic can guide an unverified trial without
// explicit coding evidence. Business errors and upstream outages never justify
// multiplying uploads or discarding a coding candidate.
func shouldProbeAfterBodyReadFailure(resp *http.Response, kind string) bool {
	return resp != nil && kind == responseFailureBodyRead &&
		(resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity)
}

const (
	responseFailureHTTP          = "http_error"
	responseFailureContentCoding = "content_coding_rejected"
	responseFailureMediaType     = "unsupported_media_type"
	responseFailureBodyRead      = "request_body_read"
)

// inspectEncodingResponse classifies a bounded diagnostic without changing the
// response delivered to the caller. Generic body-read errors are not proof of
// unsupported compression: the upstream may have received a truncated upload.
func inspectEncodingResponse(resp *http.Response) string {
	kind := responseFailureHTTP
	if resp == nil {
		return kind
	}
	if resp.StatusCode == http.StatusUnsupportedMediaType {
		kind = responseFailureMediaType
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		return kind
	}
	if _, ok := rejectionStatuses[resp.StatusCode]; !ok && kind != responseFailureMediaType {
		return kind
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRejectionBodyBytes+1))
	original := resp.Body
	reader := io.MultiReader(bytes.NewReader(body), original)
	if err != nil {
		// Inspection must not turn an upstream read error into a clean EOF.
		reader = io.MultiReader(bytes.NewReader(body), &replayReadError{err: err}, original)
	}
	resp.Body = &replayBody{reader: reader, closer: original}
	if err != nil || int64(len(body)) > maxRejectionBodyBytes {
		return kind
	}
	message := diagnosticMessage(body)
	if mentionsEncodingRejection(message) {
		return responseFailureContentCoding
	}
	if strings.Contains(strings.ToLower(message), "failed to read request body") {
		return responseFailureBodyRead
	}
	return kind
}

// Only inspect an error's message, not an echoed payload or unrelated JSON
// fields containing codec names. Never include this untrusted text in logs.
func diagnosticMessage(body []byte) string {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		// Large/HTML error pages often contain unrelated diagnostic text.
		// Only a short plain-text diagnostic can be coding evidence.
		if len(body) > 1024 || bytes.ContainsAny(body, "<>") {
			return ""
		}
		return string(body)
	}
	var nested struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
		return nested.Message
	}
	var text string
	if json.Unmarshal(envelope.Error, &text) == nil && text != "" {
		return text
	}
	if envelope.Message != "" {
		return envelope.Message
	}
	return envelope.Detail
}

func mentionsEncodingRejection(message string) bool {
	lower := strings.ToLower(message)
	// Upstream response decoding is unrelated to the submitted request coding.
	if strings.Contains(lower, "response") {
		return false
	}
	for _, keyword := range rejectionKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	if strings.Contains(lower, "decompress request") || strings.Contains(lower, "decompress the request") ||
		strings.Contains(lower, "decompress body") || strings.Contains(lower, "decompress payload") ||
		strings.Contains(lower, "decompress entity") || strings.Contains(lower, "decompress content") {
		return true
	}
	for _, coding := range []string{"brotli", "zstd", "deflate"} {
		if strings.Contains(lower, "unsupported "+coding) ||
			strings.Contains(lower, coding+" is not supported") ||
			strings.Contains(lower, "invalid "+coding) ||
			(strings.Contains(lower, coding) && strings.Contains(lower, "decode")) {
			return true
		}
	}
	return false
}

type replayReadError struct{ err error }

func (r *replayReadError) Read([]byte) (int, error) {
	if r.err == nil {
		return 0, io.EOF
	}
	err := r.err
	r.err = nil
	return 0, err
}

// replayBody serves an already-consumed prefix while keeping the original Close
// (which cancels the detached upstream context and releases the connection).
type replayBody struct {
	reader io.Reader
	closer io.Closer
}

func (b *replayBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *replayBody) Close() error {
	return b.closer.Close()
}
