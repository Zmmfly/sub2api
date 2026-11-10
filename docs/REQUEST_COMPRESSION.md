# 请求压缩状态与故障排查

## 编码保留策略

账号启用请求体压缩后，首次探测按 `br → zstd → deflate` 尝试。只有上游成功响应才确认编码可用。

已确认编码不会仅凭普通失败被永久判定为不支持。与请求体编码无关的失败（401/403/404/408/429/502/503/504/529 等认证、权限、路由、超时、限流和可用性错误）以及传输错误原样交给网关的错误处理，不在压缩层额外重发未压缩 POST，避免重复上传放大带宽。

可能由编码引起的失败（400/413/415/422/500/501，包括 ChatGPT Codex 只返回 `{"detail":"Bad Request"}` 的情况）会立即用明文重发同一请求一次。调用方只看到明文请求的结果；被丢弃的压缩尝试只记日志（含发送字节数），不计入用量流量。明文成功作为临时回退的恢复信号，该账号进入最多 **30 分钟**的明文窗口，之后重新探测压缩；它不证明编码是首次失败的唯一原因。明文也失败时不据此移除原编码，调用方收到明文请求的响应或传输错误。已有尝试之后，调用方已经断开时不再追加压缩协商或明文重试。检查使用原始客户端取消信号，即使上游上下文为了收集计费用量而分离了取消信号，也不允许断连后重放；已经在途的响应仍按原有受限读取规则完成。

HTTP 500 等响应不能保证上游没有执行请求，重放非幂等 POST 可能增加上传、重复生成或上游费用。该机制不提供幂等保证。

只有明确指向请求 Content-Encoding 不兼容的诊断才会退役已确认的编码，并尝试其它编码。裸 HTTP 415 可能是 Content-Type 不兼容，不能单独退役已确认的编码，只会触发上面的明文重发。错误 JSON 中回显的输入、图片解压错误和上游响应解压错误也不能作为请求编码被拒绝的证据。

首次探测时，任何 400/413/422/500/501（包括普通业务 400/422）都会继续尝试全部编码和一次明文请求；401/403、限流和 502/503/504 等不会触发连续重试。只有明文成功才暂时固定为非压缩；所有编码及明文都失败时保持协商未完成，后续请求可以重新执行完整探测。首次探测的裸 415 也遵循这个限制：未压缩请求同样失败时，不会把所有编码判成不支持。明确拒绝所有候选编码的上游仍会使用非压缩请求。

连续尝试全部编码只发生在尚未确认编码的首次协商；已确认编码后，同类失败最多多一次明文上传，不会再尝试其它编码。

首次探测或已确认编码的明文重发出现这些失败后，明文成功只固定 **30 分钟**，到期自动重新探测，避免偶发成功把账号永久降级为明文。明确拒绝所有候选编码后的本地明文结论不使用这个 30 分钟 TTL。配置 Redis 时，共享版本到期或被重置可重新开放探测；未配置 Redis 时则保持到本地状态重置。

配置 Redis 时，限时明文结果以 `identity` 值写入 `reqcompress:account:<account_id>:algorithm`，由 Redis 的固定 **30 分钟** TTL 决定截止时间，读取和请求不续期。每次账号请求原子读取共享值、剩余 PTTL 和版本；已经持有压缩编码的其他实例也会直接采用共享回退，不必先自行失败。重复或迟到的回退发布继承剩余时间，不重新计时。到期后清除本地计时缓存并按 `br → zstd → deflate` 重新协商，所有已升级实例使用相同的探测顺序。未配置 Redis 时仍保留本地剩余候选的优先顺序。

共享状态变化通过带版本的原子比较更新；迟到的压缩成功、明文重放、退役或重置结果不能覆盖较新的共享状态。原 algorithm 键值格式不变，新增的 `:revision` 标记保留有限时间。多实例需全部升级后，才能依赖这些跨实例保证，旧版本仍可能按旧规则写入。

Redis 读取失败或本地共享写入正忙时，仅当前请求走明文，下一请求重新读取共享状态；不因故障创建或续期本地 30 分钟窗口。Redis 写入失败时也不会把本地计时当作跨实例事实，恢复后重新读取 Redis。未配置 Redis 的单进程兼容模式仍使用本地限时回退，不具备跨实例一致性。所有候选被明确拒绝的结论只保留在本地，不写入共享 identity；配置 Redis 时，最长 6 小时的共享版本到期或重置会使其重新探测。

覆盖：`replay_test.go`、`cancellation_test.go`、`shared_cooldown_test.go`、`shared_test.go`、`preserve_test.go`，以及 repository 中的真实 HTTP 压缩重放与保留测试。

## Redis：共享编码与有限存活时间

已成功编码仍保存在 `reqcompress:account:<account_id>:algorithm`，TTL 为 **6 小时**。

- 每次账号转发请求原子读取共享状态并续期有效压缩编码，包括当前请求体太小等不适合压缩的情况；identity 的截止时间保持不变。
- 同账号重叠的读取共享一次短时快照，各请求使用独立副本；健康的并发读取不会因此大批改用明文。
- 共享状态读取采用短超时，不把本地旧编码当成 Redis 的替代来源。旧的无 TTL 压缩值在请求使用时迁移为 6 小时 TTL；管理页面查询不续期。
- 编码键到期、Redis 重启或键被清除后，重新读取并协商，避免旧本地状态重建过期回退或覆盖新结果。
- 退役和管理重置使旧版本失效；真实新协商成功后才能发布新结果。版本标记不会永久积累。

覆盖共享状态初始化、固定 identity 截止时间、迟到更新、跨实例重置及 Redis 故障恢复的测试见 `shared_test.go` 和相关回归测试。

## 用日志区分方向

| 日志/字段 | 意义 |
| --- | --- |
| `reqcompress coding preserved`，`direction=upstream`，`error_source=upstream_response` | 上游返回与编码无关的错误；保留已有编码，`identity_retry=false` |
| 同上，`error_source=upstream_transport` | 到上游的传输失败；`error_kind` 区分超时、连接重置、关闭、EOF、DNS 等，不记录可能含凭据的完整 URL 错误 |
| `reqcompress identity replay recovered`，`identity_retry=true` | 已确认编码的请求返回可能由编码引起的失败（`status`、`reason`），明文重发成功（`identity_status`）；`identity_pinned=true` 表示该账号已临时改为明文，时长见 `identity_ttl_seconds` |
| `reqcompress identity replay failed`，`identity_retry=true` | 明文重发也失败（`identity_status` 或 `identity_error_kind`），不据此否定原编码；`coding_preserved=true` 表示原编码保留 |
| `read request body failed`，`direction=downstream_ingress` | 本服务读取客户端/反向代理上传的请求体失败，发生在上游转发之前 |
| `close/flush/write response compressor`，`direction=downstream_egress` | 本服务向客户端/反向代理写响应失败；不是请求压缩协商失败 |

入站读取日志使用固定分类：`timeout`、`truncated_body`、`client_disconnect`、`decode_content_encoding`、`unsupported_content_encoding`、`max_bytes`、`capacity` 等；不记录请求正文或原始解码错误。上传超时返回 HTTP 408；损坏/截断请求仍为 400，体积限制仍为 413，缓冲容量不足仍为 503，已取消请求保持不写响应体的 499 路径。

HTTP/1 的 socket 读取超时也会导致 Go 取消请求 context，因此不能仅凭 `context.Canceled` 判定客户端主动断开。网关保留自身上传读取期限产生的原始 timeout，记录 Warn 并在响应尚未提交、连接仍可写时返回 408；真正断连或读取前已取消仍走 499。已提交的响应不会追加错误 JSON，连接已断开时也不保证客户端能收到状态码。

响应压缩中的 EPIPE、连接重置等明确下游断连记为 Info，每个请求只记录一次；真实写超时和压缩器初始化故障保持 Warn。写失败不会尝试在已输出的压缩流中混入未压缩数据。

压缩状态保留日志和明文重发失败日志按账号每分钟最多一条 Warn，其余记录在 Debug；明文重发成功日志每次都记 Warn；成功建立共享回退后，后续请求在剩余 TTL 内走明文，在途请求和 Redis 故障仍可能产生额外日志。日志带本地 `request_id`（如果请求上下文提供）和上游 `upstream_request_id`（如果响应提供），便于关联；明文重发日志另带明文请求的 `identity_upstream_request_id`。上游请求 ID 依次取 `x-request-id`、`x-oai-request-id`（ChatGPT Codex 只返回这个）、`request-id`、`x-amzn-requestid`，都没有时才用 Cloudflare 的 `cf-ray`。不要只按某一条下游 broken-pipe 日志推断账号请求压缩已失效。

覆盖：`logging_test.go`、`replay_test.go`、`httputil/request_body_error_test.go`、`handler/request_body_read_log_test.go`、`middleware/request_body_memory_test.go`、`middleware/response_compress_error_test.go`。

仅凭 `Failed to read request body` JSON 无法确定生产故障根因：它既可能由本服务入站读取生成，也可能由上游返回。需要结合上述方向、分类和同一时段的代理/客户端日志判断；保留压缩能力和补齐日志不能修复外部网络截断或上游自身故障。
