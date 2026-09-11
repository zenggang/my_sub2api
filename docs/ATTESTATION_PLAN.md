# Codex `x-oai-attestation` 适配方案

状态：HTTP、原生 WS 作用域隔离和 WS→HTTP bridge 代码已完成，默认仍为 `off`。当前分支为 `design/attestation-forwarding`，最新验证提交 `9f6636cdd`，功能实现提交 `4093453d4`，基于 `release` `5f3bfd119a9107a43669f47c5183963b6c2bb707`。已完成离线单元/集成测试、当前源码 Linux amd64 候选构建、118/18081 API-key smoke，以及当前源码通过官方 app-server + ChatGPT.app DeviceCheck provider 的真实 HTTP E2E；正式部署和官方 PR 尚未执行。原生 WS 已在当前源码候选验证入站握手、真实证明生成和 HTTP 回退，但当前账号池没有可用上游 WS 账号，WS 上游复用/转发仍保留为环境受限项。

## 1. 目标与边界

目标是让真实 Codex Desktop/app-server 已经生成的 `x-oai-attestation` 在 Sub2API 中保持可追踪、按原值传到对应 ChatGPT Codex 上游，同时不伪造、不解码、不持久化 DeviceCheck token。

链路应保持为：

```text
签名 Codex Desktop
  └─ app-server / attestation provider 生成证明
       └─ x-oai-attestation 入站
            └─ Sub2API 选择账号和上游路径
                 └─ HTTP / WS / WS→HTTP 原值透传
                      └─ chatgpt.com/backend-api/codex/responses
```

Sub2API 不负责实现 `attestation/generate`、Apple DeviceCheck、签名校验或证明刷新。服务器上的 `liveattestation` 是独立的 Live/ChatGPT App 路径，Linux 实现返回 unsupported，不能拿来给 Responses 生成证明。

这项改造不改变 Lite→5.5 兼容逻辑、不改变模型映射、不增加账号例外、不改变账号凭据、不修改 `main`，也不以“证明存在”推断账号认证或业务成功。

## 1.1 开关层级和回退语义

设备证明是跨账号、跨 transport 的上游请求安全边界，开关应放在全局 `GatewayConfig`，建议配置路径为 `gateway.openai_attestation`。不放在账号、分组或请求层：同一份客户端证明可能经过账号池切换，按账号开关会造成证明、Authorization 和连接池语义不一致；请求层开关还会让客户端自行决定代理安全策略。

建议采用枚举模式而不是单一 bool：

```yaml
gateway:
  openai_attestation:
    mode: off       # off | observe | http | all
```

| 模式 | 行为 |
| --- | --- |
| `off` | 不解析、不透传、不建立 attestation scope；现有请求路径保持旧行为 |
| `observe` | 只做脱敏观测，不向上游发送该头 |
| `http` | 只透传当前主链路的 managed HTTP、passthrough、compact；WS→HTTP bridge 和原生 WS 都保持旧行为 |
| `all` | 在 `http` 基础上开启 WS→HTTP bridge、原生 WS 握手和连接池 scope 隔离 |

安全默认值必须是 `off`。第一版不提供账号/分组例外和客户端自选开关；候选灰度通过独立候选实例或受控测试入口完成，不把生产配置切成每账号不同语义。

开关要有全局 kill switch 语义：当前 HTTP 模式切回 `off` 立即影响新建 HTTP attempt；WS 尚未开启时不需要修改 WS 连接。未来 `all` 模式切回 `off` 时，新建 HTTP/WS attempt 都关闭；已经建立的带证明上游 WS 不能热更新 Header，应立即禁止进入公共池和 prewarm，并标记/排空已有 attested 连接；默认让已有活动流完成，显式强制关闭才中断活动流。这样开关关闭后不会继续产生新的证明外发，又不会把“配置已关闭”误报为存量连接已改变。

实现上以配置文件的 `off` 作为启动默认和故障安全值；当前请求读取配置快照，尚未提供管理端热切换。`observe` 和 `http` 只影响 HTTP，`all` 才启用 WS 证明作用域。若后续接入管理端热切换，持久化配置和 runtime snapshot 必须原子更新，reload 失败保持旧快照但告警；不能因为数据库/配置中心短暂不可读而自动变成 `all`。切换日志只记录旧/新 mode、操作者、时间和受影响的连接数，不记录证明原文。

`cross_account_attempt_limit=1`、malformed 不 failover、证明相关 401/403 停止切号和 attested prewarm 禁止属于不可被普通账号/分组配置覆盖的安全硬规则；不把它们做成可随意调大的开关。

## 2. 官方事实和未决事实

官方 [Codex app-server README 的 Attestation generation](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md#attestation-generation) 和 [PR #20619](https://github.com/openai/codex/pull/20619) 给出的事实：

- Desktop 在 `initialize` 时可声明 `capabilities.requestAttestation=true`。
- app-server 按需请求 `attestation/generate`，客户端返回 `{ "token": "v1.<opaque>" }`。
- app-server 包装为 `{ "v":1, "s":0, "t":"v1.<opaque>" }`，失败状态为 `s=1/2/3/4`，无可用客户端时省略 header。
- 官方 E2E 覆盖了 HTTP POST 和 WebSocket `/backend-api/codex/responses`。
- 当前实现只在 ChatGPT Auth 且存在 attestation provider 时尝试生成；API key 不是该机制的生成来源。
- 方案依据的官方代码快照记录为合并 commit `5f4d0ec343d807f6932e6bdc5785dc5a127ac409`（PR 合并于 2026-05-08 UTC）；实现前若官方协议变化，必须重新核对 README、协议 schema 和对应 commit。

公开材料没有证明以下内容：

- `t` 是否与具体 ChatGPT 账号强绑定。
- 证明的有效期、刷新规则、是否只能使用一次。
- OAuth 账号从 A 切到 B 后，A 的证明与 B 的 Authorization 是否一定被拒绝。
- `s=1..4` 的外层状态在 OpenAI 后端的具体风控含义。

因此本方案选择“真实值原样保留并记录风险”，不自行推断上述未决事实。

## 3. 当前代码缺口

基于当前 `release` 源码的静态核对：

| 链路 | 当前位置 | 当前问题 |
| --- | --- | --- |
| managed HTTP Responses | `backend/internal/service/openai_gateway_service.go`、`openai_gateway_forward.go` | 普通 OpenAI 出站白名单没有 `x-oai-attestation`，最终构造器会丢弃入站头 |
| passthrough HTTP | `openai_gateway_service.go`、`openai_gateway_passthrough.go` | passthrough 白名单同样没有该头 |
| 原生 WS 握手 | `openai_ws_forwarder_payload.go` | WS header builder 没有接收和转发该值 |
| WS→HTTP bridge | `openai_ws_http_bridge.go` | bridge 复用 passthrough builder，但没有独立的证明作用域和连接上下文 |
| WS 连接池 | `openai_ws_pool.go` | handshake compatibility key、`lastAcquire` 和 prewarm 没有证明作用域；旧连接可能被不同下游上下文复用 |
| 观测 | gateway 日志/账本 | 当前没有证明是否存在、状态、透传一致性的脱敏字段 |

当前已存在的 `openai_live_attestation.go` 不属于上述 Responses 路径，不能通过“复用已有 live attestation”解决这些缺口。

## 4. 总体设计

新增两个彼此分离的内部对象：`AttestationForwardingContext` 和 `AttestationScope`。

`AttestationForwardingContext` 只在当前 HTTP attempt 或下游 WS 连接生命周期内存在；原始 `value` 不能进入 `openAIWSAcquireRequest.Headers`、`lastAcquire`、账号级 prewarm、数据库、Redis 或长期连接元数据。`AttestationScope` 是不可由客户端自报的本地下游连接作用域 ID，不能作为长期设备 ID。

| 字段 | 用途 | 保存边界 |
| --- | --- | --- |
| `present` | 入站是否存在 | 可记录 |
| `value` | 原始 header | 仅当前出站构造/握手，不写入池状态 |
| `version/status` | best-effort 解析外层 `v/s` | 可记录 |
| `length` | 原值字节长度 | 可记录 |
| `scope` | HTTP attempt 或 WS 下游连接作用域 | 短生命周期，连接关闭即清理 |
| `comparison` | 入站/出站是否一致 | 默认只记录一致性，不记录稳定 digest |

解析和错误规则：

1. 入站没有该头，保持没有；Sub2API 不补 envelope。
2. 单值时原样复制，包括 `s=0` 和 `s=1..4`；不解码 `t`，不重排 JSON，不重新序列化。
3. 遍历 Header map 检查大小写变体和多值。重复值、控制字符、超过实现前固定并文档化的长度上限都归类为 `malformed_attestation`：该 attempt 不发送证明，不触发账号切换或无限重试，按确定性客户端请求错误返回。
4. 未知但可传输的 envelope 保留原值；`v/s` 仅 best-effort 解析，解析失败只影响观测字段，不改变透传值。
5. HTTP 同请求重试沿用当前入站值，但每个 attempt 都重新经过目标路径和 malformed 检查；不修改入站 Header。
6. WS 证明属于下游握手 scope。证明变化必须新建上游握手；不能在旧 WebSocket 上热更新 header。

WS 池硬约束：`scope` 必须从 ingress 贯穿 `openAIWSAcquireRequest`、`openAIWSConn`、preferred/pinned/least-busy pick、handshake match、prewarm target、eviction 和 close。不同 scope 即使证明内容相同也不能复用；有证明但无 scope 时强制新建且不进入公共 idle pool。带证明连接默认关闭跨 scope prewarm；如需预热，必须由同一 scope 通过 `HeadersFactory` 重新提供当前证明，不能从缓存 Header 重拨。

## 5. 分阶段实施

### 阶段 0：只读观测

先将全局 mode 设为 `observe`，优先只观测当前 HTTP 路径，不改变上游请求。对 118 收到的真实请求增加受控、脱敏的存在性观测，至少覆盖：请求 ID、transport、target path、账号 attempt、`present`、`v/s`、长度、malformed 原因和出站是否一致。

观测要求：

- 不记录 Authorization、完整 header、`t`、DeviceCheck 原文或可直接重放的值。
- 默认只记录计数、状态和入站/出站一致性；需要短期采样时使用 keyed HMAC，不保存稳定 digest，设定 TTL、访问权限和 metrics cardinality 上限。
- 对同一 request_id 去重，区分首次 attempt、同账号重试、切换账号和最终终态。
- 观测不写业务 ledger、账号配置或 Redis；只写受控诊断日志/临时验证材料。先确认 Desktop→Sub2API 是否真的有 header，再决定透传命中率；没有入站值时不把“缺失”归因于 Sub2API 丢弃。
- 要证明上游实际收到，必须在候选上游出口或可控测试端点核对；Sub2API 自己的入站日志不能单独证明外发成功。

### 阶段 1：HTTP 原值透传

将全局 mode 从 `observe` 切到 `http` 后，只在当前主链路的 managed HTTP、passthrough HTTP 和 compact 构造点调用同一个 helper。WS→HTTP bridge 暂不纳入这一阶段。helper 必须硬检查最终 scheme、hostname、path、账号类型和 Responses 路径；判断依据不是客户端自报 Host、User-Agent 或普通 OpenAI-compatible 标签。

验收：

- 有效 `s=0`、四种失败 envelope 和未知但语法合法的 envelope 原值一致。
- 入站缺失仍缺失；API key/第三方上游不会收到该头。
- `malformed_attestation` 是确定性请求错误，不进入账号 failover、不反复重试。
- Lite→5.5 仍独立移除 Lite 标记；两者同时存在时证明保留、Lite 按原兼容规则处理。
- 失败重试和账号切换不改入站 header；每个 attempt 的观测能显示同一入站摘要。

### 阶段 2：原生 WS 握手和作用域

WS 作为后续阶段：只有将全局 mode 从 `http` 切到 `all` 后，`buildOpenAIWSHeaders` 才接收当前下游 WS 连接的证明上下文和 `AttestationScope`。握手时按阶段 1 的目标判断透传。连接池必须把“是否带证明、证明作用域”纳入兼容性判断，并让 scope 贯穿所有池操作。

建议先采用保守策略：

- 带证明的上游 WS 连接只在同一个下游 WS 连接作用域内复用；不同 scope 即使证明内容相同也不能复用。
- 在作用域尚未建立、证明存在但无法安全关联时，禁止从公共 idle pool 借连接，强制新建连接且不进入公共 pool。
- `openAIWSAcquireRequest.Headers` 和 `lastAcquire` 不保存原始证明；对带证明连接关闭跨 scope 后台 prewarm。需要预热时，必须由同一个下游作用域通过 `HeadersFactory` 重新提供当前证明，否则不预热。
- 证明值变化必须新建上游握手；不能只更新连接池 key 或在已建立的 WebSocket 上补 Header。
- 无证明的连接不能复用到有证明请求；有证明连接也不能被无证明请求借用，除非明确证明该连接作用域与目标路径完全隔离。

### 阶段 3：WS→HTTP bridge

bridge 的每个 HTTP turn 都从下游 WS 连接作用域取得证明上下文。它和原生 WS 一样属于后续 `all` 阶段，第一阶段 `http` 不修改 bridge。首版不尝试让 Sub2API 向 Desktop 请求刷新证明，也不把证明塞进 `response.create` payload。

compact/bridge 的每个 turn 继承同一 WS scope；客户端断开、上游连接关闭、failover 终止或 bridge session 清理时同时清理 scope 和证明上下文，不把它留在连接池或全局 context。metrics 不使用原始 scope 作为无限 cardinality 标签，使用受限 transport/状态枚举。

账号 A→B failover 时：

- 保留客户端这次请求携带的证明原值，向 B 的新出站 attempt 透传，并记录 `attestation_account_switch=true`。
- 不声称 A 的证明对 B 有效；如果上游返回认证/风控相关失败，按现有 failover 语义处理并单独归因。
- 带证明请求跨账号最多允许一次新账号尝试；证明/认证相关 401/403 立即停止切号，provider 503 仍按现有容量归因处理；已有语义输出后继续禁止 replay。
- 不把 A 的证明写进 B 账号、账号池或长驻连接缓存。
- 如果后续证据证明 OpenAI 要求证明和账号严格绑定，再把“带证明请求禁止跨账号 failover”设为默认策略；在证据出现前不静默丢证明或伪造 B 的证明。

### 阶段 4：候选和正式验收

先用 18081 候选，不切正式服务。HTTP 阶段候选只验证当前 HTTP 路径；WS/bridge 候选另行执行，不能因为 HTTP 候选通过就宣称 WS 适配完成。候选与线上共用 DB/Redis 时，必须关闭或隔离 prewarm、账号状态写入和调度缓存更新；如果无法证明候选只读/隔离，则不能用线上共享 DB/Redis 做 attestation 验证。仍先做迁移门禁和 active request 检查。

当前实现状态：阶段 1 的 HTTP helper 已接入 managed/passthrough/compact 构造点；阶段 2 已将证明上下文与通用 WS Headers 分离，并让 scope、证明摘要贯穿连接池的 preferred/pinned/routing/least-busy 选择，带证明连接不进入账号级 prewarm；阶段 3 的 bridge 继续复用 HTTP helper，仅在 `all` 模式转发。证明相关 malformed 请求不会进入 failover；OAuth 认证/风控 401/403 立即停止换号，其他失败在已完成一次换号后停止继续尝试。已在 118 独立 18081 候选以 API Key `id=34` 完成五模型工具调用/续聊 smoke；另用当前源码候选和官方 app-server + DeviceCheck provider 完成真实 HTTP E2E。API Key smoke 本身不生成 DeviceCheck 证明，不能替代后者。

## 6. 测试矩阵

### 单元和构造器测试

- `off`、`observe`、`http`、`all` 四种 mode 的路由矩阵；默认配置和 reload 失败都保持 `off`/旧快照，不得意外开启透传。
- `http` 模式只影响当前 HTTP 主链路；WS/bridge 仍保持旧行为。
- 从 `all` 切回 `off` 后，新请求不再透传，已有 attested WS 被禁止复用和 prewarm，活动流按 drain 语义处理。
- managed HTTP、passthrough HTTP、compact：有/无证明，原值、状态和长度一致。
- `s=0/1/2/3/4`、未知状态、重复 header、超长值、异常字符。
- malformed 不触发账号切换、同账号无限重试或共享池复用，并返回固定的确定性请求错误。
- ChatGPT Codex 目标与第三方 base URL、API key、非 Responses path 的隔离。
- Lite→5.5 与 attestation 同时存在时，证明保持、Lite metadata 仍按既有规则删除。
- 入站 header 在构造和 failover 后仍未被修改。

### WS pool 测试

- 同作用域相同证明可以复用；不同作用域即使摘要相同也不能复用。
- 有/无证明、不同证明、不同下游连接之间不能错误复用。
- `lastAcquire`、prewarm、preferred connection、force-new-connection 不泄露原始值或跨作用域复用。
- `AttestationScope` 在 acquire、preferred/pinned/least-busy、handshake match、prewarm、eviction 和 close 全路径一致；连接关闭后 scope 清理。
- 证明变化后必须重新握手，旧握手的 Header 快照不变。

### 真实候选测试

- 官方 Desktop/客户端产生真实入站证明；不伪造测试 token 冒充 DeviceCheck。
- HTTP POST、compact、原生 WS、WS→HTTP bridge。
- 工具调用和工具结果续聊到 `response.completed`，同时记录实际上游模型和账号。
- 同账号重试、账号切换、上游 503/401/风控错误分别观测；按 request_id 去重。
- 候选退出后确认正式二进制 SHA/PID 不变。

## 7. 观测与故障归因

建议新增结构化字段：

```text
attestation_present
attestation_version
attestation_status
attestation_length
attestation_malformed
attestation_scope
attestation_comparison
attestation_forwarded
attestation_account_switch
attestation_transport
```

禁止字段：原始 `x-oai-attestation`、`t`、稳定 digest prefix、完整请求头、Authorization、cookie、token。短期采样如需比较只使用 keyed HMAC，并设置 TTL、访问权限和 metrics cardinality 上限。

归因顺序：

1. 先看入站是否有证明，以及出站是否保留一致的比较结果；必要时在短期候选采样中核对 keyed HMAC。
2. 再分 provider HTTP status、重试后 200、客户端取消、本地并发拒绝。
3. 最后再比较有/无证明请求的成功率、TTFT 和风控错误；不能把单次 503 或 HTTP 200 直接归因于证明。

## 8. 官方 PR 与私有实现分界

适合向官方提 PR 的通用部分：

- HTTP/WS scoped header passthrough，目标是 `Wei-Shaw/sub2api`；不把 Codex Desktop/app-server 的 DeviceCheck 生成逻辑混入该 PR。
- 不伪造、不修改、不输出敏感值的测试和文档。
- 通用的连接作用域隔离，如果能在没有公司账号池语义的情况下独立复现。

留在 fork 私有层的部分：

- Sub2API 账号池 A→B failover 策略和诊断字段。
- 内部 API key、Guard、18081、部署与回滚工具。
- 公司账号/模型映射、指纹收敛和生产日志口径。

向官方整理贡献分支前，先在本分支完成阶段 0～2 的证据和测试；只有用户明确允许才创建或推送指向 `Wei-Shaw/sub2api` 的官方 PR。`openai/codex` 的 app-server/DeviceCheck 改动属于另一个上游，不能混在同一 PR。官方 PR 不包含内部运行地址、账号信息、部署材料或原始证明。

## 9. 实施顺序与退出条件

1. 用户评审本方案，确认是否允许阶段 0 观测。
2. 先定稿 P0 合同：证明上下文与 `Headers/lastAcquire` 分离，downstream scope 全池传递，带证明连接不跨 scope prewarm。
3. 阶段 0 只读证据达到：至少一批真实 Desktop 请求明确区分有头/无头、HTTP/WS 路径和最终 attempt，并能在可控出口证明外发结果。
4. 实现阶段 1，完成目标硬门禁、malformed 确定性错误和 HTTP 构造器隔离测试；先只在 `http` 模式候选验证和评审。
5. HTTP 候选通过后，才把 WS→HTTP bridge 和原生 WS 作为后续任务，切换 `all` 前单独完成连接池/prewarm/bridge 测试和跨账号一次上限。
6. 使用真实 Desktop 做与当前阶段匹配的隔离候选 E2E；通过后才讨论是否部署。共享 DB/Redis 无法隔离时停止候选。
7. 每阶段失败都保留候选和测试日志，回退代码提交，不修改账号配置来掩盖问题。

完成标准不是“header 已加入白名单”，而是：真实入站证明在目标路径原值到达上游；无证明和无关目标不被污染；WS 连接不跨作用域复用；账号切换风险有可观测证据；业务终态和上游归因可回查。
