package service

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/gin-gonic/gin"
)

// openAIWSTrafficCounters are monotonic per-connection totals. Message bytes
// are application-layer WebSocket payloads (after permessage-deflate
// decompression on read, before compression on write). Wire bytes are the
// payload bytes of data frames (text, binary, continuation) as they crossed
// the upgraded connection after the HTTP handshake, i.e. compressed when
// permessage-deflate applied. Frame headers, masking keys and control frames
// (ping/pong/close) are excluded, like HTTP chunk framing.
type openAIWSTrafficCounters struct {
	MessageWritten int64
	MessageRead    int64
	WireWritten    int64
	WireRead       int64
	// WriteFailures counts data-message writes that returned an error. The
	// bytes such a write reported are not evidence of a delivered message.
	WriteFailures int64
}

// openAIWSTrafficMeter is implemented by WebSocket connections that can
// attribute their traffic. wireKnown is false when the upgraded connection was
// not wrapped; message counts remain valid in that case.
type openAIWSTrafficMeter interface {
	TrafficCounters() (counters openAIWSTrafficCounters, wireKnown bool)
	TrafficCompressed() bool
}

// openAIWSTrafficStats is embedded by metered connections.
type openAIWSTrafficStats struct {
	messageWritten atomic.Int64
	messageRead    atomic.Int64
	writeFailures  atomic.Int64
	wire           *openAIWSWireCounter
	compressed     bool
}

func (s *openAIWSTrafficStats) TrafficCounters() (openAIWSTrafficCounters, bool) {
	if s == nil {
		return openAIWSTrafficCounters{}, false
	}
	counters := openAIWSTrafficCounters{
		MessageWritten: s.messageWritten.Load(),
		MessageRead:    s.messageRead.Load(),
		WriteFailures:  s.writeFailures.Load(),
	}
	if s.wire == nil {
		return counters, false
	}
	counters.WireWritten = s.wire.written.payload.Load()
	counters.WireRead = s.wire.read.payload.Load()
	return counters, true
}

func (s *openAIWSTrafficStats) TrafficCompressed() bool { return s != nil && s.compressed }

func (s *openAIWSTrafficStats) observeWrite(payloadBytes int, err error) {
	if s == nil {
		return
	}
	if err != nil {
		s.writeFailures.Add(1)
		return
	}
	s.messageWritten.Add(int64(payloadBytes))
}

func (s *openAIWSTrafficStats) observeRead(payloadBytes int) {
	if s != nil {
		s.messageRead.Add(int64(payloadBytes))
	}
}

// openAIWSWireCounter wraps the io.ReadWriteCloser that net/http returns as
// the body of a 101 response. Handshake bytes were consumed before that body
// existed, so they are never counted.
type openAIWSWireCounter struct {
	io.ReadWriteCloser
	read    openAIWSFrameMeter
	written openAIWSFrameMeter
}

func (c *openAIWSWireCounter) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	if n > 0 {
		c.read.observe(p[:n])
	}
	return n, err
}

func (c *openAIWSWireCounter) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	if n > 0 {
		c.written.observe(p[:n])
	}
	return n, err
}

// openAIWSFrameMeter follows the RFC 6455 frame boundaries of one direction of
// the byte stream and counts data-frame payload bytes, including a partially
// transferred frame. The library serializes reads and writes per direction;
// the mutex only makes that ordering explicit for the parser state.
type openAIWSFrameMeter struct {
	mu          sync.Mutex
	header      [14]byte
	headerLen   int
	headerNeed  int
	payloadLeft uint64
	control     bool
	payload     atomic.Int64
	// messages counts first frames of data messages (text or binary opcode),
	// so a caller can verify that it observed every message it attributes.
	messages atomic.Int64
}

func (m *openAIWSFrameMeter) observe(b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var data int64
	for len(b) > 0 {
		if m.payloadLeft > 0 {
			n := uint64(len(b))
			if n > m.payloadLeft {
				n = m.payloadLeft
			}
			if !m.control {
				data += int64(n)
			}
			m.payloadLeft -= n
			b = b[n:]
			continue
		}
		m.header[m.headerLen] = b[0]
		m.headerLen++
		b = b[1:]
		if m.headerLen == 2 {
			m.headerNeed = 2
			switch m.header[1] & 0x7f {
			case 126:
				m.headerNeed += 2
			case 127:
				m.headerNeed += 8
			}
			if m.header[1]&0x80 != 0 {
				m.headerNeed += 4 // masking key
			}
		}
		if m.headerLen < 2 || m.headerLen < m.headerNeed {
			continue
		}
		length := uint64(m.header[1] & 0x7f)
		switch length {
		case 126:
			length = uint64(binary.BigEndian.Uint16(m.header[2:4]))
		case 127:
			length = binary.BigEndian.Uint64(m.header[2:10])
		}
		m.control = m.header[0]&0x08 != 0 // opcodes 0x8-0xF
		if opcode := m.header[0] & 0x0f; opcode == 0x1 || opcode == 0x2 {
			m.messages.Add(1)
		}
		m.payloadLeft = length
		m.headerLen, m.headerNeed = 0, 0
	}
	if data > 0 {
		m.payload.Add(data)
	}
}

// openAIWSWireCountingTransport installs an openAIWSWireCounter on successful
// upgrades. Non-101 responses (handshake failures) pass through untouched.
type openAIWSWireCountingTransport struct {
	base    http.RoundTripper
	mu      sync.Mutex
	counter *openAIWSWireCounter
}

func (t *openAIWSWireCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusSwitchingProtocols {
		return resp, err
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		// coder/websocket rejects such a body; leave its error unchanged.
		return resp, nil
	}
	counter := &openAIWSWireCounter{ReadWriteCloser: rwc}
	resp.Body = counter
	t.mu.Lock()
	t.counter = counter
	t.mu.Unlock()
	return resp, nil
}

func (t *openAIWSWireCountingTransport) upgraded() *openAIWSWireCounter {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counter
}

// newOpenAIWSWireCountingClient returns a per-dial client sharing base's
// transport (and therefore its proxy and connection settings).
func newOpenAIWSWireCountingClient(base *http.Client) (*http.Client, *openAIWSWireCountingTransport) {
	if base == nil {
		base = http.DefaultClient
	}
	roundTripper := base.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport := &openAIWSWireCountingTransport{base: roundTripper}
	client := *base
	client.Transport = transport
	return &client, transport
}

// openAIWSPermessageDeflateNegotiated reports whether the server accepted
// permessage-deflate. coder/websocket only accepts that single extension.
func openAIWSPermessageDeflateNegotiated(headers http.Header) bool {
	for _, value := range headers.Values("Sec-WebSocket-Extensions") {
		for _, extension := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(extension, ";")
			if strings.EqualFold(strings.TrimSpace(name), "permessage-deflate") {
				return true
			}
		}
	}
	return false
}

// openAIWSTurnTraffic attributes one leased turn's upstream WebSocket traffic
// to the request collector as the difference between connection counters at
// lease start and at turn end. Idle keepalive traffic before the lease and
// after the turn ends therefore belongs to no request.
type openAIWSTurnTraffic struct {
	collector  *traffic.Collector
	meter      openAIWSTrafficMeter
	start      openAIWSTrafficCounters
	wireKnown  bool
	readFailed atomic.Bool
	once       sync.Once
	finished   atomic.Bool
}

// newOpenAIWSTurnTraffic snapshots conn's counters for one turn. A finalized
// collector receives nothing. A connection without a meter cannot be
// attributed, so the collector's upstream totals become unknown instead of
// silently omitting this attempt.
func newOpenAIWSTurnTraffic(collector *traffic.Collector, conn any) *openAIWSTurnTraffic {
	if collector == nil || collector.Finalized() {
		return nil
	}
	meter, ok := conn.(openAIWSTrafficMeter)
	if !ok || meter == nil {
		collector.ExcludeUpstream()
		return nil
	}
	start, wireKnown := meter.TrafficCounters()
	return &openAIWSTurnTraffic{collector: collector, meter: meter, start: start, wireKnown: wireKnown}
}

// openAIWSTrafficCollector prefers the call context and falls back to the
// gin request, matching how the HTTP forwarders resolve their collector.
func openAIWSTrafficCollector(ctx context.Context, c *gin.Context) *traffic.Collector {
	if collector := traffic.FromContext(ctx); collector != nil {
		return collector
	}
	if c != nil && c.Request != nil {
		return traffic.FromContext(c.Request.Context())
	}
	return nil
}

// beginTraffic starts per-turn attribution. A finalized collector (for
// example a hijacked WebSocket ingress request) receives nothing. A connection
// without a meter cannot be attributed, so the request's upstream totals become
// unknown instead of silently omitting this attempt.
func (l *openAIWSConnLease) beginTraffic(collector *traffic.Collector) {
	if l == nil || l.conn == nil {
		return
	}
	if tracker := newOpenAIWSTurnTraffic(collector, l.conn.ws); tracker != nil {
		l.traffic.Store(tracker)
	}
}

// beginTurnTraffic attributes a client WebSocket turn on a lease that can stay
// checked out across turns. Attempts of the same turn on this lease share one
// tracker. A tracker left by an earlier turn is published to that turn's
// collector first (a no-op once that turn has finalized). The returned tracker
// must be finished when the turn ends; nil means nothing is attributed.
func (l *openAIWSConnLease) beginTurnTraffic(collector *traffic.Collector) *openAIWSTurnTraffic {
	if l == nil || l.conn == nil || collector == nil {
		return nil
	}
	if current := l.traffic.Load(); current != nil {
		if current.collector == collector && !current.finished.Load() {
			return current
		}
		current.finish()
	}
	l.beginTraffic(collector)
	if current := l.traffic.Load(); current != nil && current.collector == collector && !current.finished.Load() {
		return current
	}
	return nil
}

// noteTrafficReadFailure records that this turn's reader observed a failed
// read: the connection may have consumed part of a message that was never
// decoded.
func (l *openAIWSConnLease) noteTrafficReadFailure(err error) {
	if l == nil || err == nil {
		return
	}
	if tracker := l.traffic.Load(); tracker != nil {
		tracker.readFailed.Store(true)
	}
}

// finishTraffic publishes the turn once. It runs before the socket is closed or
// returned to the pool, so close handshakes and later idle pings are excluded.
func (l *openAIWSConnLease) finishTraffic() {
	if l == nil {
		return
	}
	if tracker := l.traffic.Load(); tracker != nil {
		tracker.finish()
	}
}

func (t *openAIWSTurnTraffic) finish() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		defer t.finished.Store(true)
		end, wireKnown := t.meter.TrafficCounters()
		compressed := t.meter.TrafficCompressed()
		wireKnown = wireKnown && t.wireKnown
		delta := func(before, after int64) *int64 {
			n := after - before
			if n < 0 {
				return nil
			}
			return &n
		}
		var requestOriginal, requestActual *int64
		if end.WriteFailures == t.start.WriteFailures {
			requestOriginal = delta(t.start.MessageWritten, end.MessageWritten)
			if wireKnown {
				requestActual = delta(t.start.WireWritten, end.WireWritten)
			}
		}
		responseOriginal := delta(t.start.MessageRead, end.MessageRead)
		var responseActual *int64
		if wireKnown {
			responseActual = delta(t.start.WireRead, end.WireRead)
		}
		switch {
		case !compressed && wireKnown:
			// Without permessage-deflate no frame may be compressed (RSV1 is
			// rejected), so the consumed payload prefix is both representations,
			// including a partially read message, as for identity HTTP bodies.
			requestOriginal, responseOriginal = requestActual, responseActual
		case compressed && t.readFailed.Load():
			// Same rule as an encoded HTTP body ending early: the compressed wire
			// prefix and the decoded messages need not describe the same data.
			responseOriginal = nil
		}
		t.collector.Record(traffic.UpstreamRequest, requestOriginal, requestActual, compressed)
		t.collector.Record(traffic.UpstreamResponse, responseOriginal, responseActual, compressed)
	})
}
