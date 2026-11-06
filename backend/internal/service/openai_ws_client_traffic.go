package service

import (
	"bufio"
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

// OpenAIWSClientTraffic measures a WebSocket that a client opened to the
// gateway (WebSocket ingress) and attributes it to turns. A turn starts when
// the gateway reads the client's request for it and ends when its terminal
// event has been written or the turn fails. Each turn owns a traffic.Collector
// that also receives the turn's upstream attempts and is finalized exactly once.
//
// The downstream counts follow the upstream WebSocket contract: original bytes
// are application message payloads (read after permessage-deflate
// decompression, written before compression); actual bytes are data-frame
// payload bytes on the hijacked connection after the 101 handshake. Frame
// headers, masking keys and control frames are excluded. Bytes outside an
// active turn (keepalive, close handshake, messages between turns) belong to
// no turn.
//
// All methods are safe on a nil receiver: reads and writes still reach the
// connection, and no turn is attributed.
type OpenAIWSClientTraffic struct {
	stats openAIWSTrafficStats
	// messagesWritten counts data messages written successfully. Together
	// with the wire meter's message count it detects a write path that
	// bypassed the meter, so its payload cannot silently go missing.
	messagesWritten atomic.Int64
	readFailures    atomic.Int64
	accountID       atomic.Int64

	mu sync.Mutex
	// readMark is the connection position from which the next request is
	// read: the start of the read in progress or the end of the last turn,
	// whichever is later.
	readMark  openAIWSClientTrafficMark
	turn      *openAIWSClientTurn
	closed    bool
	onTurnEnd func(OpenAIWSClientTurnTraffic)
}

// OpenAIWSClientTurnTraffic is published once per finalized turn.
type OpenAIWSClientTurnTraffic struct {
	// Usage is the turn's frozen snapshot, upstream and downstream.
	Usage       *traffic.Usage
	FinalizedAt time.Time
	// RequestID is the usage_logs.request_id of the turn's usage row, or empty
	// when the turn produced none.
	RequestID string
	// AccountID is the upstream account selected when the turn ended, or 0.
	AccountID int64
}

type openAIWSClientTrafficMark struct {
	openAIWSTrafficCounters
	wireKnown           bool
	wireMessagesWritten int64
	messagesWritten     int64
	readFailures        int64
}

type openAIWSClientTurn struct {
	collector *traffic.Collector
	start     openAIWSClientTrafficMark
	requestID string
	// finishers publish upstream attempts that are still open when the turn
	// ends, before the collector is frozen.
	finishers []func()
}

// NewOpenAIWSClientTraffic returns a meter for one client connection. Install
// it with WrapResponseWriter before coderws.Accept.
func NewOpenAIWSClientTraffic() *OpenAIWSClientTraffic {
	return &OpenAIWSClientTraffic{}
}

type openAIWSClientTrafficContextKey struct{}

// WithOpenAIWSClientTraffic carries the connection meter to the ingress
// forwarders and to helpers that write client events.
func WithOpenAIWSClientTraffic(ctx context.Context, t *OpenAIWSClientTraffic) context.Context {
	if ctx == nil || t == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIWSClientTrafficContextKey{}, t)
}

// OpenAIWSClientTrafficFromContext returns the connection meter, or nil.
func OpenAIWSClientTrafficFromContext(ctx context.Context) *OpenAIWSClientTraffic {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(openAIWSClientTrafficContextKey{}).(*OpenAIWSClientTraffic)
	return t
}

// WriteOpenAIWSClientMessage writes one client data message and counts it on
// the meter carried by ctx, if any.
func WriteOpenAIWSClientMessage(ctx context.Context, conn *coderws.Conn, messageType coderws.MessageType, payload []byte) error {
	return OpenAIWSClientTrafficFromContext(ctx).WriteClientMessage(ctx, conn, messageType, payload)
}

// WrapResponseWriter returns w with a Hijack that meters the upgraded
// connection. coderws.Accept must receive the returned writer.
func (t *OpenAIWSClientTraffic) WrapResponseWriter(w gin.ResponseWriter) gin.ResponseWriter {
	if t == nil || w == nil {
		return w
	}
	return &openAIWSClientTrafficWriter{ResponseWriter: w, traffic: t}
}

type openAIWSClientTrafficWriter struct {
	gin.ResponseWriter
	traffic *OpenAIWSClientTraffic
}

// Hijack runs after net/http flushed the 101 response, so the returned
// connection carries only WebSocket frames.
func (w *openAIWSClientTrafficWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.Hijack()
	if err != nil || conn == nil || rw == nil {
		return conn, rw, err
	}
	return w.traffic.attach(conn, rw, openAIWSPermessageDeflateNegotiated(w.Header()))
}

// openAIWSClientWireConn counts the frames of the hijacked connection. prefix
// holds bytes that the HTTP server had already buffered after the request
// headers; they are WebSocket frames too and pass through the read meter.
type openAIWSClientWireConn struct {
	net.Conn
	wire   *openAIWSWireCounter
	prefix []byte
}

func (c *openAIWSClientWireConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		c.wire.read.observe(p[:n])
		return n, nil
	}
	return c.wire.Read(p)
}

func (c *openAIWSClientWireConn) Write(p []byte) (int, error) { return c.wire.Write(p) }

func (t *OpenAIWSClientTraffic) attach(conn net.Conn, rw *bufio.ReadWriter, compressed bool) (net.Conn, *bufio.ReadWriter, error) {
	// A fresh hijack writer is empty; flush defensively so handshake bytes can
	// never be counted as frames.
	if err := rw.Writer.Flush(); err != nil {
		return conn, rw, err
	}
	wire := &openAIWSWireCounter{ReadWriteCloser: conn}
	metered := &openAIWSClientWireConn{Conn: conn, wire: wire}
	if n := rw.Reader.Buffered(); n > 0 {
		buffered, _ := rw.Reader.Peek(n)
		metered.prefix = append([]byte(nil), buffered...)
	}
	// Accept runs before any reader or writer of this connection exists.
	t.stats.wire = wire
	t.stats.compressed = compressed
	reader := bufio.NewReaderSize(metered, rw.Reader.Size())
	writer := bufio.NewWriterSize(metered, rw.Writer.Size())
	return metered, bufio.NewReadWriter(reader, writer), nil
}

// Compressed reports whether the client negotiated permessage-deflate.
func (t *OpenAIWSClientTraffic) Compressed() bool {
	return t != nil && t.stats.TrafficCompressed()
}

// ReadClientMessage is ReadOpenAIWSClientMessage with the read attributed to
// this connection's meter.
func (t *OpenAIWSClientTraffic) ReadClientMessage(
	controlCtx context.Context,
	conn *coderws.Conn,
	timeout time.Duration,
	timeoutStatus coderws.StatusCode,
	timeoutReason string,
) (coderws.MessageType, []byte, error) {
	return readOpenAIWSClientMessageMetered(controlCtx, conn, timeout, timeoutStatus, timeoutReason, nil, nil, t)
}

// WriteClientMessage writes one data message to the client and counts it.
func (t *OpenAIWSClientTraffic) WriteClientMessage(ctx context.Context, conn *coderws.Conn, messageType coderws.MessageType, payload []byte) error {
	err := conn.Write(ctx, messageType, payload)
	t.observeWrite(len(payload), err)
	return err
}

func (t *OpenAIWSClientTraffic) observeWrite(payloadBytes int, err error) {
	if t == nil {
		return
	}
	t.stats.observeWrite(payloadBytes, err)
	if err == nil {
		t.messagesWritten.Add(1)
	}
}

// beforeRead marks where the next request starts. A turn still in progress
// keeps its own baseline; EndTurn moves the mark past it.
func (t *OpenAIWSClientTraffic) beforeRead() {
	if t == nil {
		return
	}
	mark := t.mark()
	t.mu.Lock()
	t.readMark = laterOpenAIWSClientTrafficMark(t.readMark, mark)
	t.mu.Unlock()
}

func (t *OpenAIWSClientTraffic) afterRead(payloadBytes int, err error) {
	if t == nil {
		return
	}
	if err == nil {
		t.stats.observeRead(payloadBytes)
		return
	}
	// A close frame ends the read cleanly; any other error can leave a
	// partially consumed message that was never decoded.
	if coderws.CloseStatus(err) == -1 {
		t.readFailures.Add(1)
	}
}

func (t *OpenAIWSClientTraffic) mark() openAIWSClientTrafficMark {
	counters, wireKnown := t.stats.TrafficCounters()
	mark := openAIWSClientTrafficMark{
		openAIWSTrafficCounters: counters,
		wireKnown:               wireKnown,
		messagesWritten:         t.messagesWritten.Load(),
		readFailures:            t.readFailures.Load(),
	}
	if wire := t.stats.wire; wire != nil {
		mark.wireMessagesWritten = wire.written.messages.Load()
	}
	return mark
}

// laterOpenAIWSClientTrafficMark returns the later read position. Counters are
// monotonic, so this is a per-field maximum of the read side.
func laterOpenAIWSClientTrafficMark(current, candidate openAIWSClientTrafficMark) openAIWSClientTrafficMark {
	if candidate.MessageRead < current.MessageRead || candidate.WireRead < current.WireRead {
		return current
	}
	return candidate
}

// SetOnTurnEnd registers the callback that receives every finalized turn. It
// runs on the goroutine that ended the turn.
func (t *OpenAIWSClientTraffic) SetOnTurnEnd(callback func(OpenAIWSClientTurnTraffic)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onTurnEnd = callback
	t.mu.Unlock()
}

// SetAccountID records the upstream account now serving the connection.
func (t *OpenAIWSClientTraffic) SetAccountID(accountID int64) {
	if t != nil {
		t.accountID.Store(accountID)
	}
}

// BeginTurn starts a turn for the request that was just read. A turn that is
// still open ends first. It returns nil after Close.
func (t *OpenAIWSClientTraffic) BeginTurn() *traffic.Collector {
	if t == nil {
		return nil
	}
	t.EndTurn()
	now := t.mark()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	start := now
	// The request's own frames were read before this call.
	start.MessageRead = t.readMark.MessageRead
	start.WireRead = t.readMark.WireRead
	start.readFailures = t.readMark.readFailures
	t.turn = &openAIWSClientTurn{collector: traffic.NewCollector(), start: start}
	return t.turn.collector
}

// TurnCollector returns the active turn's collector, or nil.
func (t *OpenAIWSClientTraffic) TurnCollector() *traffic.Collector {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == nil {
		return nil
	}
	return t.turn.collector
}

// TurnContext places the active turn's collector in ctx, so upstream HTTP
// attempts made with it are measured by traffic.NewUpstreamTransport.
func (t *OpenAIWSClientTraffic) TurnContext(ctx context.Context) context.Context {
	if collector := t.TurnCollector(); collector != nil {
		return traffic.WithCollector(ctx, collector)
	}
	return ctx
}

// SetTurnRequestID records the request ID of the active turn's usage row.
func (t *OpenAIWSClientTraffic) SetTurnRequestID(requestID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.turn != nil {
		t.turn.requestID = requestID
	}
	t.mu.Unlock()
}

// addTurnFinisher runs finish before the active turn is frozen. It reports
// false when no turn is active.
func (t *OpenAIWSClientTraffic) addTurnFinisher(finish func()) bool {
	if t == nil || finish == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn == nil {
		return false
	}
	t.turn.finishers = append(t.turn.finishers, finish)
	return true
}

// beginUpstreamTurnTraffic attributes the active turn's attempt on an upstream
// WebSocket connection that is not a pooled lease.
func (t *OpenAIWSClientTraffic) beginUpstreamTurnTraffic(conn any) *openAIWSTurnTraffic {
	tracker := newOpenAIWSTurnTraffic(t.TurnCollector(), conn)
	if tracker != nil && !t.addTurnFinisher(tracker.finish) {
		return nil
	}
	return tracker
}

// beginLeaseTurnTraffic attributes the active turn's attempt on a pooled
// lease, which may stay checked out across turns.
func (t *OpenAIWSClientTraffic) beginLeaseTurnTraffic(lease *openAIWSConnLease) {
	if tracker := lease.beginTurnTraffic(t.TurnCollector()); tracker != nil {
		t.addTurnFinisher(tracker.finish)
	}
}

// EndTurn publishes and finalizes the active turn, if any.
func (t *OpenAIWSClientTraffic) EndTurn() {
	if t == nil {
		return
	}
	t.mu.Lock()
	turn := t.turn
	t.turn = nil
	onTurnEnd := t.onTurnEnd
	t.mu.Unlock()
	if turn == nil {
		return
	}
	for _, finish := range turn.finishers {
		finish()
	}
	end := t.mark()
	t.mu.Lock()
	// Everything read so far belongs to this turn; a later request starts here.
	t.readMark = laterOpenAIWSClientTrafficMark(t.readMark, end)
	requestID := turn.requestID
	t.mu.Unlock()
	turn.record(end, t.Compressed())
	turn.collector.Finalize()
	if onTurnEnd != nil {
		onTurnEnd(OpenAIWSClientTurnTraffic{
			Usage:       turn.collector.Snapshot(),
			FinalizedAt: turn.collector.FinalizedAt(),
			RequestID:   requestID,
			AccountID:   t.accountID.Load(),
		})
	}
}

// Close ends the active turn and stops attributing later turns.
func (t *OpenAIWSClientTraffic) Close() {
	if t == nil {
		return
	}
	t.EndTurn()
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (turn *openAIWSClientTurn) record(end openAIWSClientTrafficMark, compressed bool) {
	start := turn.start
	wireKnown := start.wireKnown && end.wireKnown
	delta := func(before, after int64) *int64 {
		n := after - before
		if n < 0 {
			return nil
		}
		return &n
	}
	requestOriginal := delta(start.MessageRead, end.MessageRead)
	var requestActual, responseOriginal, responseActual *int64
	if wireKnown {
		requestActual = delta(start.WireRead, end.WireRead)
	}
	// A failed client write is no evidence of delivered bytes.
	if end.WriteFailures == start.WriteFailures {
		responseOriginal = delta(start.MessageWritten, end.MessageWritten)
		if wireKnown {
			responseActual = delta(start.WireWritten, end.WireWritten)
		}
	}
	switch {
	case !compressed && wireKnown:
		// Without permessage-deflate no frame is compressed, so the consumed
		// payload is both representations, including a partial frame.
		requestOriginal = requestActual
		if responseActual != nil {
			responseOriginal = responseActual
		}
	case compressed:
		if end.readFailures != start.readFailures {
			// A failed read can consume deflated frames it never decoded.
			requestOriginal = nil
		}
		if wireKnown && end.wireMessagesWritten-start.wireMessagesWritten != end.messagesWritten-start.messagesWritten {
			// A message reached the wire without being counted here.
			responseOriginal = nil
		}
	}
	turn.collector.Record(traffic.DownstreamRequest, requestOriginal, requestActual, compressed)
	turn.collector.Record(traffic.DownstreamResponse, responseOriginal, responseActual, compressed)
}
