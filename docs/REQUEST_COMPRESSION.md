# 请求压缩状态与故障排查

## 编码保留策略

账号启用请求体压缩后，首次探测按 `br → zstd → deflate` 尝试。只有上游成功响应才确认编码可用。

已经确认的编码不会因为普通 HTTP 错误、超时、断连，或泛化的 `Failed to read request body` 响应而被禁用。这些失败原样交给网关的错误处理，不在压缩层额外重发未压缩 POST，避免重复上传放大带宽。

只有明确指向请求 Content-Encoding 不兼容的诊断才会退役已确认的编码，并尝试其它编码。裸 HTTP 415 可能是 Content-Type 不兼容，不能单独推翻已确认的编码。错误 JSON 中回显的输入、图片解压错误和上游响应解压错误也不能作为请求编码被拒绝的证据。

首次探测时，普通业务错误、403、限流和上游 5xx 不会消耗候选编码或触发连续重试。未确认编码收到特定的 400/422 `Failed to read request body`，或 ChatGPT Codex 严格只有 `{"detail":"Bad Request"}` 的模糊 400 时，可以继续尝试其它编码；如果所有这类尝试都失败，只有未压缩重试成功才会暂时固定为非压缩。首次探测的裸 415 也遵循这个限制：未压缩请求同样失败时，不会把所有编码判成不支持。明确拒绝所有候选编码的上游仍会使用非压缩请求。

上述两种模糊 400 只允许首次协商继续探测；已确认编码后再次出现相同响应，仍按普通 HTTP 错误处理，不做未压缩重放。带其它字段的 400 或普通业务 400 不享受这个例外。

模糊失败后明文成功只固定 **30 分钟**，到期自动重新探测剩余候选，避免偶发成功把账号永久降级为明文。明确拒绝所有候选编码后固定明文不受这个 TTL 影响，因为它代表已经确认的能力结论。

这种限时明文结果以 `identity` 值写入同一个 Redis algorithm 键，TTL 固定为 30 分钟，读取和请求不会续期。各实例通过原子读取值与 PTTL 对齐同一个截止时间；后启动的实例只继承剩余时间。重复发布不会延长已有 identity，另一实例已确认的压缩结果也不会被迟到的 identity 覆盖。到期后重新协商成功会用普通压缩结果及 6 小时滑动 TTL 覆盖它。明确拒绝产生的永久明文不写 Redis。

覆盖：`TestExecutePreservesSettledCodingOnOrdinaryHTTPFailures`、`TestExecutePreservesSettledCodingOnTransportFailure`、`TestExecuteInitialHealthFailuresDoNotMultiplyTrafficOrPinIdentity`、`TestExecuteInitialBodyReadFailureOnlyPinsIdentityAfterSuccessfulPlainAttempt`、`TestExecuteProbesIdentityAfterInitialGenericCodexBadRequest`、`TestExecuteBodyReadIdentityRetriesAfterTTL`、`TestPersistSharedIdentityScriptPreservesCodingAndDeadline`、`TestRegistrySharesAmbiguousIdentityAcrossInstancesUntilTTL`、`TestUpstreamRequestCompressionGenericCodex400FallsBackToIdentity`、`TestUpstreamRequestCompressionPreservesVerifiedCodingAcrossHTTPFailures`。

## Redis：6 小时滑动过期

已成功编码保存在 `reqcompress:account:<account_id>:algorithm`，TTL 为 **6 小时**。

- 每次账号转发请求触发续期，包括当前请求体太小等不适合压缩的情况；同账号同时在途的续期合并，不让请求排队等 Redis。
- 旧的无 TTL 编码在请求使用时迁移为 6 小时 TTL；管理页面查询不会续期。
- 编码选择保留在本地内存。Redis 续期失败不会禁用已成功编码；续期采用短超时，失败日志按账号限频。
- TTL 到期或 Redis 重启导致键缺失时，仍持有有效本地编码的实例可以恢复该键；不会覆盖另一个实例发布的不同编码。
- 显式退役或管理重置留下 6 小时的失效标记，阻止旧实例的缺键恢复和迟到续期复活旧结果；新的真实协商成功后清除标记。

覆盖：`shared_test.go` 的 `TestRegistryRefreshSharedAlgorithm*`、`TestRegistrySettledAlgorithmReadDoesNotRenewTTL` 和 `preserve_test.go` 的 `TestExecuteRefreshesSharedCodingForEveryAccountRequest`。

## 用日志区分方向

| 日志/字段 | 意义 |
| --- | --- |
| `reqcompress coding preserved`，`direction=upstream`，`error_source=upstream_response` | 上游返回错误；保留已有编码，`identity_retry=false` |
| 同上，`error_source=upstream_transport` | 到上游的传输失败；`error_kind` 区分超时、连接重置、关闭、EOF、DNS 等，不记录可能含凭据的完整 URL 错误 |
| `read request body failed`，`direction=downstream_ingress` | 本服务读取客户端/反向代理上传的请求体失败，发生在上游转发之前 |
| `close/flush/write response compressor`，`direction=downstream_egress` | 本服务向客户端/反向代理写响应失败；不是请求压缩协商失败 |

入站读取日志使用固定分类：`timeout`、`truncated_body`、`client_disconnect`、`decode_content_encoding`、`unsupported_content_encoding`、`max_bytes`、`capacity` 等；不记录请求正文或原始解码错误。上传超时返回 HTTP 408；损坏/截断请求仍为 400，体积限制仍为 413，缓冲容量不足仍为 503，已取消请求保持不写响应体的 499 路径。

HTTP/1 的 socket 读取超时也会导致 Go 取消请求 context，因此不能仅凭 `context.Canceled` 判定客户端主动断开。网关保留自身上传读取期限产生的原始 timeout，记录 Warn 并在响应尚未提交、连接仍可写时返回 408；真正断连或读取前已取消仍走 499。已提交的响应不会追加错误 JSON，连接已断开时也不保证客户端能收到状态码。

响应压缩中的 EPIPE、连接重置等明确下游断连记为 Info，每个请求只记录一次；真实写超时和压缩器初始化故障保持 Warn。写失败不会尝试在已输出的压缩流中混入未压缩数据。

压缩状态保留日志按账号每分钟最多一条 Warn，其余记录在 Debug；日志带本地 `request_id`（如果请求上下文提供）和上游 `upstream_request_id`（如果响应提供），便于关联。不要只按某一条下游 broken-pipe 日志推断账号请求压缩已失效。

覆盖：`logging_test.go`、`httputil/request_body_error_test.go`、`handler/request_body_read_log_test.go`、`middleware/request_body_memory_test.go`、`middleware/response_compress_error_test.go`。

仅凭 `Failed to read request body` JSON 无法确定生产故障根因：它既可能由本服务入站读取生成，也可能由上游返回。需要结合上述方向、分类和同一时段的代理/客户端日志判断；保留压缩能力和补齐日志不能修复外部网络截断或上游自身故障。
