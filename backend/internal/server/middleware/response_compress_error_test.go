package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/compresslevel"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// docs/REQUEST_COMPRESSION.md 的下游错误归因、去重与日志脱敏由本文件覆盖。
// compressErrorPayloadMarker 放进响应体但不放进任何日志字段，用于断言
// 压缩失败日志不会泄露响应内容。
const compressErrorPayloadMarker = "compress-error-payload-marker-9f2c"

// compressFaultWriter 垫在 gin 之下，模拟下游连接已经断开：Write 可按需
// 返回失败，模拟压缩器向客户端写字节时遇到的 EPIPE/超时等错误。
type compressFaultWriter struct {
	header   http.Header
	body     bytes.Buffer
	writeErr error
	flushes  int
}

func (w *compressFaultWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *compressFaultWriter) WriteHeader(int) {}

func (w *compressFaultWriter) Write(data []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(data)
}

func (w *compressFaultWriter) Flush() {
	w.flushes++
}

// compressFailurePlan 通过请求 context 传给测试 handler，描述本次请求
// 应在哪个环节触发下游失败，以及记录 handler 观察到的写出结果。
type compressFailurePlan struct {
	phase       string
	failErr     error
	writeString bool
	body        []byte
	fault       *compressFaultWriter
	collector   *traffic.Collector
	cancelCtx   bool

	writes   int
	writeN   int
	writeErr error
}

type compressFailurePlanKey struct{}

func newCompressFailureEngine(t *testing.T, providers ...compresslevel.Provider) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(ResponseCompress(providers...))
	engine.GET("/compress", func(c *gin.Context) {
		plan, ok := c.Request.Context().Value(compressFailurePlanKey{}).(*compressFailurePlan)
		if !ok || plan == nil || plan.fault == nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		switch plan.phase {
		case compressPhaseWrite:
			plan.fault.writeErr = plan.failErr
			plan.writeN, plan.writeErr = writeFailurePlanBody(c, plan)
			plan.writes++
			// 同一断连下的重复写出、Flush 与后续 Close 都不应再产生日志。
			_, _ = c.Writer.Write([]byte("second write after downstream failure"))
			plan.writes++
			c.Writer.Flush()
		case compressPhaseFlush:
			plan.writeN, plan.writeErr = writeFailurePlanBody(c, plan)
			plan.writes++
			plan.fault.writeErr = plan.failErr
			c.Writer.Flush()
		case compressPhaseClose:
			plan.writeN, plan.writeErr = writeFailurePlanBody(c, plan)
			plan.writes++
			plan.fault.writeErr = plan.failErr
		default:
			plan.writeN, plan.writeErr = writeFailurePlanBody(c, plan)
			plan.writes++
		}
	})
	return engine
}

func writeFailurePlanBody(c *gin.Context, plan *compressFailurePlan) (int, error) {
	if plan.writeString {
		return c.Writer.WriteString(string(plan.body))
	}
	return c.Writer.Write(plan.body)
}

func runCompressFailureRequest(t *testing.T, engine *gin.Engine, plan *compressFailurePlan, acceptEncoding string) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	requestLogger := zap.New(core).With(zap.String("request_id", "compress-error-test-request"))
	ctx := logger.IntoContext(context.Background(), requestLogger)
	if plan.collector != nil {
		ctx = traffic.WithCollector(ctx, plan.collector)
	}
	ctx = context.WithValue(ctx, compressFailurePlanKey{}, plan)
	if plan.cancelCtx {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	request := httptest.NewRequest(http.MethodGet, "/compress", nil)
	request = request.WithContext(ctx)
	request.Header.Set(headerAcceptEncoding, acceptEncoding)
	engine.ServeHTTP(plan.fault, request)
	return logs
}

// compressLogErrorField 取出 zap.Error 字段的原始 error，用于断言日志
// 保留了可供 errors.Is 判断的底层错误链。
func compressLogErrorField(t *testing.T, entry observer.LoggedEntry) error {
	t.Helper()
	for _, field := range entry.Context {
		if field.Key != "error" {
			continue
		}
		err, ok := field.Interface.(error)
		require.Truef(t, ok, "error field has type %T, want error", field.Interface)
		return err
	}
	t.Fatal("log entry has no error field")
	return nil
}

func assertCompressLogDoesNotLeakPayload(t *testing.T, entry observer.LoggedEntry) {
	t.Helper()
	require.NotContains(t, entry.Message, compressErrorPayloadMarker)
	for key, value := range entry.ContextMap() {
		require.NotContains(t, fmt.Sprint(value), compressErrorPayloadMarker,
			"log field %q leaks response payload", key)
	}
}

// incompressibleCompressPayload 生成确定性的高熵数据：brotli 在写出约一个
// 窗口（4 MiB）后才会真正向下游写字节，高熵输入可确保 Write 阶段就能触发
// 底层写失败，而不是拖到 Flush/Close。
func incompressibleCompressPayload(size int) []byte {
	payload := make([]byte, size)
	state := uint32(0x9e3779b9)
	for i := range payload {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		payload[i] = byte(state)
	}
	copy(payload, compressErrorPayloadMarker)
	return payload
}

// TestResponseCompressDownstreamDisconnectLoggedOnceAtInfo 通过 Gin 与真实
// 压缩器覆盖 br/gzip/deflate 的 Write、WriteString、Flush、Close 失败：
// 每个请求只应产生一条 Info 级 client_disconnect 日志，且方向、编码、阶段、
// 底层错误齐全，不泄露响应体。
func TestResponseCompressDownstreamDisconnectLoggedOnceAtInfo(t *testing.T) {
	writePayload := incompressibleCompressPayload(4 << 20)
	bufferedPayload := []byte(compressErrorPayloadMarker + strings.Repeat("event: buffered stream data\n", 64))

	tests := []struct {
		name        string
		phase       string
		writeString bool
		body        []byte
		wantMessage string
	}{
		{name: "write", phase: compressPhaseWrite, body: writePayload, wantMessage: "write response compressor"},
		{name: "write_string", phase: compressPhaseWrite, writeString: true, body: writePayload, wantMessage: "write response compressor"},
		{name: "flush", phase: compressPhaseFlush, body: bufferedPayload, wantMessage: "flush response compressor"},
		{name: "close", phase: compressPhaseClose, body: bufferedPayload, wantMessage: "close response compressor"},
	}

	for _, encoding := range []string{"br", "gzip", "deflate"} {
		for _, tt := range tests {
			t.Run(encoding+"/"+tt.name, func(t *testing.T) {
				engine := newCompressFailureEngine(t)
				plan := &compressFailurePlan{
					phase:       tt.phase,
					failErr:     syscall.EPIPE,
					writeString: tt.writeString,
					body:        tt.body,
					fault:       &compressFaultWriter{},
				}

				logs := runCompressFailureRequest(t, engine, plan, encoding)

				entries := logs.All()
				require.Len(t, entries, 1, "downstream disconnect must be logged exactly once per request")
				entry := entries[0]
				require.Equal(t, zapcore.InfoLevel, entry.Level)
				require.Equal(t, tt.wantMessage, entry.Message)

				fields := entry.ContextMap()
				require.Equal(t, "http.response_compress", fields["component"])
				require.Equal(t, encoding, fields["encoding"])
				require.Equal(t, tt.phase, fields["phase"])
				require.Equal(t, compressErrorKindClientDisconnect, fields["error_kind"])
				require.Equal(t, compressDirectionDownstreamEgress, fields["direction"])
				// request context 上的日志字段必须原样保留，便于关联具体请求。
				require.Equal(t, "compress-error-test-request", fields["request_id"])
				require.ErrorIs(t, compressLogErrorField(t, entry), syscall.EPIPE)
				assertCompressLogDoesNotLeakPayload(t, entry)
				// 断连只记录日志，不得偷偷切换为未压缩重写。
				require.Equal(t, encoding, plan.fault.header.Get("Content-Encoding"),
					"downstream disconnect must not rewrite the response uncompressed")

				if tt.phase == compressPhaseWrite {
					// Write 返回的错误不能被日志逻辑吞掉，后续重复失败也不追加日志。
					require.ErrorIs(t, plan.writeErr, syscall.EPIPE)
					require.Equal(t, 2, plan.writes)
				} else {
					require.NoError(t, plan.writeErr, "body must be accepted before the %s failure", tt.phase)
				}
			})
		}
	}
}

// TestResponseCompressNextRequestAfterDisconnect 验证断连请求只影响自身：
// 同一引擎上的下一个请求仍按新的 Accept-Encoding 正常压缩，Content-Encoding
// 不残留上一次失败请求的编码，body 可完整解压。
func TestResponseCompressNextRequestAfterDisconnect(t *testing.T) {
	engine := newCompressFailureEngine(t)
	encodings := [][2]string{
		{"br", "gzip"},
		{"gzip", "deflate"},
		{"deflate", "br"},
	}

	for _, pair := range encodings {
		failureEncoding, successEncoding := pair[0], pair[1]
		t.Run(failureEncoding+"_then_"+successEncoding, func(t *testing.T) {
			failedCollector := traffic.NewCollector()
			failedCollector.Begin(traffic.DownstreamResponse, false)
			failed := &compressFailurePlan{
				phase:     compressPhaseClose,
				failErr:   syscall.ECONNRESET,
				body:      []byte(compressErrorPayloadMarker + " first response"),
				fault:     &compressFaultWriter{},
				collector: failedCollector,
			}
			failedLogs := runCompressFailureRequest(t, engine, failed, failureEncoding)
			require.Len(t, failedLogs.All(), 1)
			require.Equal(t, zapcore.InfoLevel, failedLogs.All()[0].Level)
			require.Nil(t, failedCollector.Snapshot().Downstream.Response.OriginalBytes,
				"failed close must keep the request traffic measurement incomplete")

			successBody := compressibleBody() + compressErrorPayloadMarker
			successCollector := traffic.NewCollector()
			successCollector.Begin(traffic.DownstreamResponse, false)
			success := &compressFailurePlan{
				body:      []byte(successBody),
				fault:     &compressFaultWriter{},
				collector: successCollector,
			}
			successLogs := runCompressFailureRequest(t, engine, success, successEncoding)

			require.Zero(t, successLogs.Len(), "healthy request must not log compression failures")
			require.NoError(t, success.writeErr)
			require.Equal(t, successEncoding, success.fault.header.Get("Content-Encoding"))
			require.Equal(t, successBody, decompressCompressedBody(t, successEncoding, success.fault.body.Bytes()))
			usage := successCollector.Snapshot()
			require.NotNil(t, usage.Downstream.Response.OriginalBytes,
				"complete close on a healthy request must preserve compressed-body accounting")
			require.Equal(t, int64(len(successBody)), *usage.Downstream.Response.OriginalBytes)
		})
	}
}

// TestResponseCompressInitFailureStaysWarn 验证真正的压缩器初始化失败仍是
// Warn，不会被下游断连分类误降级或静音；即使请求 ctx 已被外层取消
// （如 WithWriteTimeout），init 失败也必须保持 codec_error + Warn。
func TestResponseCompressInitFailureStaysWarn(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "active_context"
		if canceled {
			name = "canceled_context"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			provider := compresslevel.ProviderFunc(func(context.Context) compresslevel.Settings {
				settings := compresslevel.Default()
				settings.Gzip = 99 // gzip.NewWriterLevel 会拒绝的非法级别
				return settings
			})
			engine := gin.New()
			engine.Use(ResponseCompress(provider))
			body := compressErrorPayloadMarker + strings.Repeat("plain body after codec failure\n", 8)
			engine.GET("/compress", func(c *gin.Context) {
				if _, err := c.Writer.WriteString(body); err != nil {
					t.Errorf("passthrough write: %v", err)
				}
			})

			core, logs := observer.New(zap.DebugLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			if canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			request := httptest.NewRequest(http.MethodGet, "/compress", nil)
			request = request.WithContext(ctx)
			request.Header.Set(headerAcceptEncoding, "gzip")
			fault := &compressFaultWriter{}
			engine.ServeHTTP(fault, request)

			entries := logs.All()
			require.Len(t, entries, 1)
			entry := entries[0]
			require.Equal(t, zapcore.WarnLevel, entry.Level)
			require.Equal(t, "create response compressor", entry.Message)
			fields := entry.ContextMap()
			require.Equal(t, compressErrorKindCodec, fields["error_kind"])
			require.Equal(t, compressPhaseInit, fields["phase"])
			require.Equal(t, "gzip", fields["encoding"])
			require.NotContains(t, fields, "direction")
			err := compressLogErrorField(t, entry)
			require.Error(t, err)
			require.Contains(t, err.Error(), "compression level")

			require.Empty(t, fault.header.Get("Content-Encoding"))
			require.Equal(t, body, fault.body.String())
		})
	}
}

// compressWriteTimeoutWriter 垫在 httputil.WithWriteTimeout 之下：设置写
// 期限后，Write 返回真实的 deadline 错误，writeTimeoutWriter.fail 会因此
// cancel 请求 ctx，用于验证超时优先于 ctx 取消兜底。
type compressWriteTimeoutWriter struct {
	header   http.Header
	body     bytes.Buffer
	deadline time.Time
}

func (w *compressWriteTimeoutWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *compressWriteTimeoutWriter) WriteHeader(int) {}

func (w *compressWriteTimeoutWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (w *compressWriteTimeoutWriter) Write(data []byte) (int, error) {
	if !w.deadline.IsZero() {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	return w.body.Write(data)
}

func (w *compressWriteTimeoutWriter) Flush() {}

// TestResponseCompressWriteTimeoutStaysWarnOnCanceledContext 覆盖真实
// httputil.WithWriteTimeout 路径：writeTimeoutWriter.fail 在 deadline 错误
// 后会 cancel 请求 ctx，日志仍须按 err 自身的超时属性归为
// downstream_timeout + Warn，而不是 client_disconnect + Info。
func TestResponseCompressWriteTimeoutStaysWarnOnCanceledContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(ResponseCompress())
	var requestCtxErr error
	engine.GET("/timeout", func(c *gin.Context) {
		_, writeErr := c.Writer.WriteString(compressErrorPayloadMarker + " write timeout body")
		requestCtxErr = c.Request.Context().Err()
		if writeErr == nil {
			t.Errorf("write should fail with the write deadline")
		}
	})

	handler := httputil.WithWriteTimeout(engine, 20*time.Millisecond)
	core, logs := observer.New(zap.DebugLevel)
	request := httptest.NewRequest(http.MethodGet, "/timeout", nil)
	request = request.WithContext(logger.IntoContext(request.Context(), zap.New(core)))
	request.Header.Set(headerAcceptEncoding, "gzip")
	sink := &compressWriteTimeoutWriter{}
	handler.ServeHTTP(sink, request)

	entries := logs.All()
	require.Len(t, entries, 1)
	entry := entries[0]
	require.Equal(t, zapcore.WarnLevel, entry.Level)
	require.Equal(t, "write response compressor", entry.Message)
	fields := entry.ContextMap()
	require.Equal(t, compressErrorKindDownstreamTimeout, fields["error_kind"])
	require.NotEqual(t, compressErrorKindClientDisconnect, fields["error_kind"])
	require.Equal(t, compressDirectionDownstreamEgress, fields["direction"])
	require.Equal(t, compressPhaseWrite, fields["phase"])
	require.Equal(t, "gzip", fields["encoding"])
	require.ErrorIs(t, compressLogErrorField(t, entry), os.ErrDeadlineExceeded)
	// writeTimeoutWriter.fail 已 cancel ctx，证明分类未落入 ctx 取消兜底。
	require.ErrorIs(t, requestCtxErr, context.Canceled)
	require.Equal(t, "gzip", sink.header.Get("Content-Encoding"))
	assertCompressLogDoesNotLeakPayload(t, entry)
}

// blockingError 模拟超时错误，实现 net.Error 供分类测试使用。
type compressTimeoutError struct{}

func (compressTimeoutError) Error() string   { return "i/o timeout" }
func (compressTimeoutError) Timeout() bool   { return true }
func (compressTimeoutError) Temporary() bool { return true }

// TestLogCompressErrorClassification 覆盖所有下游断连哨兵、请求 context 已
// 取消、下游超时与真正的压缩器错误，确保只有明确的下游失效才降级。
func TestLogCompressErrorClassification(t *testing.T) {
	genericWriteErr := errors.New("downstream socket write failed")
	codecErr := errors.New("flate: internal compressor failure")

	tests := []struct {
		name          string
		phase         string
		err           error
		cancelContext bool
		wantLevel     zapcore.Level
		wantKind      string
		wantDirection bool
	}{
		{name: "epipe", phase: compressPhaseWrite, err: syscall.EPIPE, wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "wrapped_epipe", phase: compressPhaseClose, err: fmt.Errorf("write tcp 127.0.0.1:443: %w", syscall.EPIPE), wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "econnreset", phase: compressPhaseFlush, err: syscall.ECONNRESET, wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "net_err_closed", phase: compressPhaseClose, err: net.ErrClosed, wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "io_err_closed_pipe", phase: compressPhaseWrite, err: io.ErrClosedPipe, wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "context_canceled", phase: compressPhaseClose, err: context.Canceled, wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "request_context_canceled", phase: compressPhaseWrite, err: genericWriteErr, cancelContext: true, wantLevel: zapcore.InfoLevel, wantKind: compressErrorKindClientDisconnect, wantDirection: true},
		{name: "canceled_context_net_write_timeout", phase: compressPhaseWrite, err: &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}, cancelContext: true, wantLevel: zapcore.WarnLevel, wantKind: compressErrorKindDownstreamTimeout, wantDirection: true},
		{name: "net_timeout", phase: compressPhaseFlush, err: compressTimeoutError{}, wantLevel: zapcore.WarnLevel, wantKind: compressErrorKindDownstreamTimeout, wantDirection: true},
		{name: "context_deadline_exceeded", phase: compressPhaseClose, err: context.DeadlineExceeded, wantLevel: zapcore.WarnLevel, wantKind: compressErrorKindDownstreamTimeout, wantDirection: true},
		{name: "generic_write_failure", phase: compressPhaseWrite, err: genericWriteErr, wantLevel: zapcore.WarnLevel, wantKind: compressErrorKindCodec},
		{name: "codec_error", phase: compressPhaseInit, err: codecErr, wantLevel: zapcore.WarnLevel, wantKind: compressErrorKindCodec},
		{name: "codec_error_with_canceled_context", phase: compressPhaseInit, err: codecErr, cancelContext: true, wantLevel: zapcore.WarnLevel, wantKind: compressErrorKindCodec},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			ctx := logger.IntoContext(context.Background(), zap.New(core))
			if tt.cancelContext {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			logCompressError(ctx, "compressor failure", "gzip", tt.phase, tt.err)

			entries := logs.All()
			require.Len(t, entries, 1)
			entry := entries[0]
			require.Equal(t, tt.wantLevel, entry.Level)
			require.Equal(t, "compressor failure", entry.Message)
			fields := entry.ContextMap()
			require.Equal(t, "http.response_compress", fields["component"])
			require.Equal(t, "gzip", fields["encoding"])
			require.Equal(t, tt.phase, fields["phase"])
			require.Equal(t, tt.wantKind, fields["error_kind"])
			require.ErrorIs(t, compressLogErrorField(t, entry), tt.err)
			if tt.wantDirection {
				require.Equal(t, compressDirectionDownstreamEgress, fields["direction"])
			} else {
				require.NotContains(t, fields, "direction")
			}
		})
	}
}
