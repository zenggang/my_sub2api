# Codex `x-oai-attestation` 适配方案

状态：方案评审分支。当前分支为 `design/attestation-forwarding`，基于 `release` `5f3bfd119a9107a43669f47c5183963b6c2bb707`。本分支只交付方案，不包含实现代码、生产配置、账号变更、候选部署或官方 PR。

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

## 2. 官方事实和未决事实

官方 [Codex app-server README 的 Attestation generation](https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md#attestation-generation) 和 [PR #20619](https://github.com/openai/codex/pull/20619) 给出的事实：

- Desktop 在 `initialize` 时可声明 `capabilities.requestAttestation=true`。
- app-server 按需请求 `attestation/generate`，客户端返回 `{ "token": "v1.<opaque>" }`。
- app-server 包装为 `{ "v":1, "s":0, "t":"v1.<opaque>" }`，失败状态为 `s=1/2/3/4`，无可用客户端时省略 header。
- 官方 E2E 覆盖了 HTTP POST 和 WebSocket `/backend-api/codex/responses`。
- 当前实现只在 ChatGPT Auth 且存在 attestation provider 时尝试生成；API key 不是该机制的生成来源。

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

新增一个只负责转发边界的内部概念：`AttestationForwardingContext`。它不保存原文 token 到数据库或 Redis，生命周期不超过一次请求或一次下游 WS 连接。

建议字段：

| 字段 | 用途 | 是否进入日志 |
| --- | --- | --- |
| `present` | 入站是否存在 | 是 |
| `value` | 原始 header，仅存在于当前出站构造上下文 | 否 |
| `version` | 可解析时取外层 `v` | 是 |
| `status` | 可解析时取外层 `s` | 是 |
| `length` | 原值字节长度 | 是 |
| `digest` | 内存中的短 SHA-256 摘要，用于同一次请求/连接比较 | 仅短期诊断，禁止作为长期设备 ID |
| `scope` | 下游 HTTP attempt 或 WS 连接作用域 | 是，使用内部 request/connection ID |

解析规则：

1. 入站没有该头，保持没有；Sub2API 不补默认 envelope。
2. 只有一个 header value 时原样复制，包括 `s=0` 和 `s=1..4`；不解码 `t`，不重排 JSON，不重新序列化。
3. 出现重复 header value、超过合理长度或明显控制字符时，不拼接、不选择第一项；该 attempt 不向上游发送，并记录 `malformed`。具体长度上限在实现前根据 Go transport 和官方请求实测确定，不能凭经验硬编码。
4. HTTP 重试/同请求切号沿用入站原值；不把它写回入站请求，不把本次值放进账号记录、Redis、数据库或跨请求缓存。
5. WS 证明属于下游握手连接作用域。新握手重新接收；证明改变时不能在旧上游握手上热更新 header。

## 5. 分阶段实施

### 阶段 0：只读观测

先不改变上游请求。对 118 收到的真实请求增加受控、脱敏的存在性观测，至少覆盖：请求 ID、transport、target path、账号 attempt、`present`、`v/s`、长度、短摘要和 malformed 原因。

观测要求：

- 不记录 Authorization、完整 header、`t`、DeviceCheck 原文或可直接重放的值。
- 只在已配置的诊断日志级别记录短摘要；默认日志保留计数和状态。
- 对同一 request_id 去重，区分首次 attempt、同账号重试、切换账号和最终终态。
- 先确认 Desktop→Sub2API 是否真的有 header，再决定透传命中率；没有入站值时不把“缺失”归因于 Sub2API 丢弃。

### 阶段 1：HTTP 原值透传

在最终目标已确定为 ChatGPT Codex Responses 的 managed HTTP、passthrough HTTP 和 compact 构造点调用同一个 helper。判断依据是最终 URL/协议和账号类型，不是客户端自报 Host、User-Agent 或普通 OpenAI-compatible 标签。

验收：

- 有效 `s=0`、四种失败 envelope 和未知但语法合法的 envelope 原值一致。
- 入站缺失仍缺失；API key/第三方上游不会收到该头。
- Lite→5.5 仍独立移除 Lite 标记；两者同时存在时证明保留、Lite 按原兼容规则处理。
- 失败重试和账号切换不改入站 header；每个 attempt 的观测能显示同一入站摘要。

### 阶段 2：原生 WS 握手和作用域

`buildOpenAIWSHeaders` 接收当前下游 WS 连接的证明上下文。握手时按阶段 1 的目标判断透传。连接池必须把“是否带证明、证明作用域”纳入兼容性判断。

建议先采用保守策略：

- 带证明的上游 WS 连接只在同一个下游 WS 连接作用域内复用。
- 在作用域尚未建立、证明存在但无法安全关联时，禁止从公共 idle pool 借连接，强制新建连接。
- `lastAcquire` 不保存原始证明；对带证明连接不做跨作用域后台 prewarm。需要预热时，必须由同一个下游作用域重新提供证明，否则关闭该连接的 prewarm。
- 证明值变化必须新建上游握手；不能只更新连接池 key 或在已建立的 WebSocket 上补 Header。
- 无证明的连接不能复用到有证明请求；有证明连接也不能被无证明请求借用，除非明确证明该连接作用域与目标路径完全隔离。

### 阶段 3：WS→HTTP bridge

bridge 的每个 HTTP turn 都从下游 WS 连接作用域取得证明上下文。首版不尝试让 Sub2API 向 Desktop 请求刷新证明，也不把证明塞进 `response.create` payload。

账号 A→B failover 时：

- 保留客户端这次请求携带的证明原值，向 B 的新出站 attempt 透传，并记录 `attestation_account_switch=true`。
- 不声称 A 的证明对 B 有效；如果上游返回认证/风控相关失败，按现有 failover 语义处理并单独归因。
- 不把 A 的证明写进 B 账号、账号池或长驻连接缓存。
- 如果后续证据证明 OpenAI 要求证明和账号严格绑定，再增加“带证明请求禁止跨账号 failover”的显式策略开关；在证据出现前不静默丢证明或伪造 B 的证明。

### 阶段 4：候选和正式验收

先用 18081 候选，不切正式服务。候选与线上共用 DB/Redis，仍先做迁移门禁和 active request 检查。

## 6. 测试矩阵

### 单元和构造器测试

- managed HTTP、passthrough HTTP、compact：有/无证明，原值、状态和长度一致。
- `s=0/1/2/3/4`、未知状态、重复 header、超长值、异常字符。
- ChatGPT Codex 目标与第三方 base URL、API key、非 Responses path 的隔离。
- Lite→5.5 与 attestation 同时存在时，证明保持、Lite metadata 仍按既有规则删除。
- 入站 header 在构造和 failover 后仍未被修改。

### WS pool 测试

- 同作用域相同证明可以复用；不同作用域即使摘要相同也不能复用。
- 有/无证明、不同证明、不同下游连接之间不能错误复用。
- `lastAcquire`、prewarm、preferred connection、force-new-connection 不泄露原始值或跨作用域复用。
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
attestation_digest_prefix
attestation_forwarded
attestation_account_switch
attestation_transport
```

禁止字段：原始 `x-oai-attestation`、`t`、完整请求头、Authorization、cookie、token。

归因顺序：

1. 先看入站是否有证明，以及出站是否保留同一短摘要。
2. 再分 provider HTTP status、重试后 200、客户端取消、本地并发拒绝。
3. 最后再比较有/无证明请求的成功率、TTFT 和风控错误；不能把单次 503 或 HTTP 200 直接归因于证明。

## 8. 官方 PR 与私有实现分界

适合向官方提 PR 的通用部分：

- HTTP/WS scoped header passthrough。
- 不伪造、不修改、不输出敏感值的测试和文档。
- 通用的连接作用域隔离，如果能在没有公司账号池语义的情况下独立复现。

留在 fork 私有层的部分：

- Sub2API 账号池 A→B failover 策略和诊断字段。
- 内部 API key、Guard、18081、部署与回滚工具。
- 公司账号/模型映射、指纹收敛和生产日志口径。

向官方整理贡献分支前，先在本分支完成阶段 0～2 的证据和测试；只有用户明确允许才创建或推送官方 PR。官方 PR 不包含内部运行地址、账号信息、部署材料或原始证明。

## 9. 实施顺序与退出条件

1. 用户评审本方案，确认是否允许阶段 0 观测。
2. 阶段 0 只读证据达到：至少一批真实 Desktop 请求明确区分有头/无头、HTTP/WS 路径和最终 attempt。
3. 实现阶段 1，完成构造器与隔离测试。
4. 实现阶段 2～3，完成连接池/prewarm/bridge 测试。
5. 使用真实 Desktop 做 18081 候选 E2E；通过后才讨论是否部署。
6. 每阶段失败都保留候选和测试日志，回退代码提交，不修改账号配置来掩盖问题。

完成标准不是“header 已加入白名单”，而是：真实入站证明在目标路径原值到达上游；无证明和无关目标不被污染；WS 连接不跨作用域复用；账号切换风险有可观测证据；业务终态和上游归因可回查。
