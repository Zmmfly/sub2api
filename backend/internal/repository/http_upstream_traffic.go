package repository

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
)

// Instrument beneath Grok's fallback so rejected attempts, redirects, and
// fallback responses are measured too, not just the final successful body.
func httpClientWithTraffic(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if fallback, ok := base.(*grokAccessDeniedFallbackTransport); ok {
		clone.Transport = &grokAccessDeniedFallbackTransport{base: &trafficUpstreamTransport{base: fallback.base}}
	} else {
		clone.Transport = &trafficUpstreamTransport{base: base}
	}
	return &clone
}

type trafficUpstreamTransport struct{ base http.RoundTripper }

func (t *trafficUpstreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	collector := traffic.FromContext(request.Context())
	if collector == nil {
		return t.base.RoundTrip(request)
	}
	// Clone headers, not the client's request. Explicit negotiation prevents
	// net/http's implicit gzip decoder from hiding the actual entity bytes.
	// Preserve explicit negotiation, including repeated/empty field lines.
	req := request.Clone(request.Context())
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if values := req.Header.Values("Accept-Encoding"); len(values) > 0 {
		accept := strings.Join(values, ",")
		if strings.TrimSpace(accept) == "" {
			accept = "identity"
		}
		// Go's transport only checks Header.Get; canonicalize the list so an
		// empty first field cannot hide a later explicit coding or trigger gzip.
		req.Header.Set("Accept-Encoding", accept)
	} else if req.Header.Get("Range") == "" && req.Method != http.MethodHead {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	compressed := traffic.IsCompressed(traffic.ContentEncoding(req.Header))
	current := collector.Begin(traffic.UpstreamRequest, compressed)
	currentResponse := collector.Begin(traffic.UpstreamResponse, false)
	written := make(map[*traffic.Counter]bool)
	var mu sync.Mutex
	wrap := func(body io.ReadCloser, counter *traffic.Counter) io.ReadCloser {
		expected := req.ContentLength
		if expected == 0 && body != nil && body != http.NoBody {
			expected = -1
		}
		return traffic.ObserveBody(body, counter, !compressed, expected)
	}
	noBody := req.Body == nil || req.Body == http.NoBody
	if !noBody {
		req.Body = wrap(req.Body, current)
	}
	if originalGetBody := req.GetBody; originalGetBody != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			body, err := originalGetBody()
			if err != nil {
				return nil, err
			}
			mu.Lock()
			current = collector.Begin(traffic.UpstreamRequest, compressed)
			currentResponse = collector.Begin(traffic.UpstreamResponse, false)
			counter := current
			mu.Unlock()
			return wrap(body, counter), nil
		}
	}
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		mu.Lock()
		counter := current
		written[counter] = info.Err == nil
		mu.Unlock()
		if info.Err != nil {
			// A reader can be consumed before the socket write fails. Its length
			// is not evidence of bytes actually sent; do not publish a false sum.
			counter.FailActual()
			counter.FailOriginal()
			return
		}
		counter.CompleteActual()
		if !compressed {
			counter.CompleteOriginal()
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		mu.Lock()
		if !written[current] {
			current.FailActual()
			current.FailOriginal()
		}
		mu.Unlock()
		return resp, err
	}
	if noBody {
		mu.Lock()
		current.CompleteActual()
		if !compressed {
			current.CompleteOriginal()
		}
		mu.Unlock()
	}
	if resp == nil {
		return resp, nil
	}
	encoding := traffic.ContentEncoding(resp.Header)
	responseCompressed := traffic.IsCompressed(encoding) || resp.Uncompressed
	mu.Lock()
	counter := currentResponse
	mu.Unlock()
	if responseCompressed {
		counter.SetCompressed()
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		collector.ExcludeUpstream()
		return resp, nil
	}
	if req.Method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified || resp.Body == nil || resp.Body == http.NoBody {
		counter.CompleteActual()
		counter.CompleteOriginal()
		return resp, nil
	}
	if resp.Uncompressed {
		// Nonstandard transports may already have decoded. Original is still
		// measurable; encoded bytes are irretrievably unknown, never fabricated.
		resp.Body = traffic.ObserveOriginal(resp.Body, counter)
		return resp, nil
	}
	resp.Body = traffic.ObserveBody(resp.Body, counter, !responseCompressed, resp.ContentLength)
	if responseCompressed {
		decompressResponseBodyLazy(resp, counter)
	}
	return resp, nil
}
