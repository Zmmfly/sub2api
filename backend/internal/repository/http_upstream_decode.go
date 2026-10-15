package repository

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// decompressResponseBodyLazy advertises the decoded representation without
// reading the entity. In particular, merely receiving gzip/deflate/zstd headers
// must not wait for the first body byte or defeat ResponseHeaderTimeout.
// Unsupported/stacked encodings stay opaque and retain their original headers.
func decompressResponseBodyLazy(resp *http.Response, counter *traffic.Counter) {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(traffic.ContentEncoding(resp.Header)))
	switch encoding {
	case "gzip", "x-gzip", "br", "deflate", "zstd":
	default:
		return
	}
	encoded := *resp
	encoded.Header = resp.Header.Clone()
	resp.Body = &lazyDecompressedBody{encoded: &encoded, counter: counter, encoding: encoding}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
}

// The public doUpstreamRequest wrapper serializes Read/Close and cancels the
// transport before Close. Grok's internal fallback reads/closes sequentially.
// Close here never initializes a decoder and never drains unread body data.
type lazyDecompressedBody struct {
	encoded     *http.Response
	counter     *traffic.Counter
	encoding    string
	body        io.ReadCloser
	initialized bool
	initErr     error
}

func (b *lazyDecompressedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !b.initialized {
		b.initialized = true
		if decoded, err := decodeResponseBody(b.encoded); !decoded {
			// Supported but malformed coding must not be returned as raw bytes
			// with already-normalized decoded headers. Match implicit Go gzip:
			// decoding errors surface from Read, never from receiving headers.
			b.counter.FailOriginal()
			if err == nil {
				err = fmt.Errorf("invalid compressed body")
			}
			b.initErr = fmt.Errorf("decode Content-Encoding %q: %w", b.encoding, err)
			b.body = b.encoded.Body
		} else {
			b.body = traffic.ObserveOriginal(b.encoded.Body, b.counter)
		}
	}
	if b.initErr != nil {
		return 0, b.initErr
	}
	return b.body.Read(p)
}

func (b *lazyDecompressedBody) Close() error {
	if b.body != nil {
		return b.body.Close()
	}
	return b.encoded.Body.Close()
}
