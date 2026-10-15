package traffic

import (
	"io"
	"strings"
	"sync"
)

// IsCompressed reports a content coding other than identity. Unknown codings
// are still compressed representations, but their original length is unknown.
func IsCompressed(encoding string) bool {
	encoding = strings.TrimSpace(encoding)
	return encoding != "" && !strings.EqualFold(encoding, "identity")
}

// ObserveBody counts actual reads, never Content-Length alone. A matching known
// length can establish completion without requiring a speculative extra Read.
// identity means the same bytes also measure the original representation.
func ObserveBody(body io.ReadCloser, counter *Counter, identity bool, expected int64) io.ReadCloser {
	if counter == nil || body == nil {
		return body
	}
	return &observedBody{ReadCloser: body, counter: counter, identity: identity, expected: expected}
}

type observedBody struct {
	io.ReadCloser
	counter  *Counter
	identity bool
	expected int64
	mu       sync.Mutex
	read     int64
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.read += int64(n)
	b.counter.AddActual(n)
	if b.identity {
		b.counter.AddOriginal(n)
	}
	if err != nil && err != io.EOF {
		b.counter.FailActual()
		if b.identity {
			b.counter.FailOriginal()
		}
	} else if err == io.EOF || (b.expected >= 0 && b.read == b.expected) {
		b.counter.CompleteActual()
		if b.identity {
			b.counter.CompleteOriginal()
		}
	}
	return n, err
}

// ObserveOriginal is placed after a content decoder, before protocol transforms.
func ObserveOriginal(body io.ReadCloser, counter *Counter) io.ReadCloser {
	if body == nil || counter == nil {
		return body
	}
	return &originalBody{ReadCloser: body, counter: counter}
}

type originalBody struct {
	io.ReadCloser
	counter *Counter
}

func (b *originalBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.counter.AddOriginal(n)
	if err == io.EOF {
		b.counter.CompleteOriginal()
	} else if err != nil {
		b.counter.FailOriginal()
	}
	return n, err
}
