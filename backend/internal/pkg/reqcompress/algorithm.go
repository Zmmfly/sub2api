package reqcompress

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Algorithm is an HTTP content-coding token used for a compressed request body.
type Algorithm string

const (
	// AlgorithmNone is the settled state "this account falls back to identity".
	AlgorithmNone Algorithm = ""
	// AlgorithmBrotli is the HTTP "br" content coding.
	AlgorithmBrotli Algorithm = "br"
	// AlgorithmZstd is the HTTP "zstd" content coding.
	AlgorithmZstd Algorithm = "zstd"
	// AlgorithmDeflate is the HTTP "deflate" content coding (zlib wrapper).
	AlgorithmDeflate Algorithm = "deflate"
)

// TrialOrder is the fixed preference order used while negotiating. Brotli
// usually saves the most bytes; zstd/deflate cover providers that only accept
// the simpler codings.
var TrialOrder = []Algorithm{AlgorithmBrotli, AlgorithmZstd, AlgorithmDeflate}

// sharedZstdEncoder amortizes the encoder's large window allocation across
// requests. EncodeAll is documented as safe for concurrent use.
var sharedZstdEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
})

// encodeBody compresses body with the given coding. HTTP "deflate" is the zlib
// wrapped format (RFC 9110 references RFC 1950), not a raw DEFLATE stream.
func encodeBody(algorithm Algorithm, body []byte) ([]byte, error) {
	var buf bytes.Buffer
	switch algorithm {
	case AlgorithmBrotli:
		writer := brotli.NewWriterLevel(&buf, brotli.DefaultCompression)
		if _, err := writer.Write(body); err != nil {
			return nil, fmt.Errorf("reqcompress: brotli encode: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("reqcompress: brotli close: %w", err)
		}
	case AlgorithmZstd:
		encoder, err := sharedZstdEncoder()
		if err != nil {
			return nil, fmt.Errorf("reqcompress: zstd writer: %w", err)
		}
		return encoder.EncodeAll(body, nil), nil
	case AlgorithmDeflate:
		writer := zlib.NewWriter(&buf)
		if _, err := writer.Write(body); err != nil {
			return nil, fmt.Errorf("reqcompress: deflate encode: %w", err)
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("reqcompress: deflate close: %w", err)
		}
	default:
		return nil, fmt.Errorf("reqcompress: unsupported algorithm %q", algorithm)
	}
	return buf.Bytes(), nil
}
