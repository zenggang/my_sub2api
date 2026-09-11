# `x-oai-attestation` 验证记录

本记录对应分支 `design/attestation-forwarding` 的当前实现。原始证明值、API Key 内容和 Authorization 从不写入本文件。

## 代码与离线验证

- 源码 HEAD：`6f219544d`（已推送到 `origin/design/attestation-forwarding`）。当前源码候选二进制对应功能实现提交：`4093453d4`；其后的提交只增加了验证材料，未改变 HTTP/WS 转发核心。
- `go test ./...`：通过。
- 前端 `pnpm build`：通过。
- Linux amd64：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -trimpath`，通过。
- 当前源码本地候选二进制 SHA-256：`7abc13e4af8f220881a46effa5b0a3093362fb5b03c9878e7f92cb0eeaaf8387`。

## 118 隔离候选

候选路径为 `/data/sub2api-patched/attestation-candidates/2f653d5e4/sub2api`，运行时设置 `SERVER_HOST=127.0.0.1`、`SERVER_PORT=18081`、`GATEWAY_OPENAI_ATTESTATION_MODE=all`。候选使用独立临时 unit `sub2api-attestation-canary.service`，验证结束后已停止并确认 18081 端口释放。

使用 `gang.zeng1@zhaogang.com` 名下 API Key `id=34`（只在 118 本机由 smoke 脚本读取）执行了五个模型，每个模型两轮 Responses 工具调用和续聊：

| 模型 | 结果 |
| --- | --- |
| `gpt-5.5` | 两轮 `response.completed`，工具调用和 `hello` 续聊通过 |
| `gpt-5.6-sol` | 两轮 `response.completed`，工具调用和 `hello` 续聊通过 |
| `gpt-5.6-terra` | 两轮 `response.completed`，工具调用和 `hello` 续聊通过 |
| `gpt-5.6-luna` | 两轮 `response.completed`，工具调用和 `hello` 续聊通过 |
| `gpt-6-astra` | 两轮 `response.completed`，工具调用和 `hello` 续聊通过；期间出现一次上游 503，同账号重试后完成 |

候选停止前后正式实例均为：

- 正式二进制 SHA-256：`9378ba6db053081fcd03a8968c63f0f7a7d1116f46c99e47e8f761e8c1c2020b`。
- `sub2api.service` PID：`25190`。
- `18080/health`：通过。

本轮再次使用 `gang.zeng1@zhaogang.com` 名下 API Key（内部 ID `34`）复验候选 `2f653d5e4`，启动时设置 `GATEWAY_OPENAI_ATTESTATION_MODE=all`，候选只监听 `127.0.0.1:18081`。五个模型均完成两轮 `response.completed`，第一轮工具调用和第二轮 `hello` 续聊通过；候选随后自动停止。复验后 `sub2api-attestation-canary.service` 为 inactive/unknown、18081 已释放，正式实例仍为 SHA `9378ba6d…`、PID `25190`，`18080/health` 返回 `{"status":"ok"}`。

随后用当前源码候选 `4093453d4`（二进制 SHA-256 `7abc13e4af8f220881a46effa5b0a3093362fb5b03c9878e7f92cb0eeaaf8387`）重复隔离验证，结果相同：五个模型十轮请求全部完成。该候选使用相同的 `all` 模式，仅监听 `127.0.0.1:18081`，未替换正式二进制。

本次候选与正式实例共用运行环境中的 DB/Redis；候选未切换正式服务、未替换正式二进制。启动时正式实例存在活动请求，因此没有使用 `--allow-active` 或重启正式服务；候选仅绑定 18081，验证后立即停止。正式部署仍需独立的活动请求和迁移门禁。

## 证据边界

API Key smoke 证明候选在 `all` 配置下的 HTTP Responses、模型映射、工具续聊和正式实例隔离行为；API Key 客户端自身不会生成 DeviceCheck 证明。真实 Desktop HTTP E2E 已在当前源码候选上完成；compact、原生 WS 上游复用和 WS→HTTP bridge 尚未具备完整真实上游证据。

正式部署和向官方提交 PR 均未执行。

## 官方 CLI 观测

使用 `/Applications/ChatGPT.app/Contents/Resources/codex`（`codex-cli 0.153.4`，本机 ChatGPT 登录状态）通过 SSH 隧道访问 118 候选 18081，候选设置为 `mode=observe`。请求返回 HTTP 200；候选 Info 日志记录：

```text
openai_attestation_observed transport=http target=codex present=false value_count=0
```

这证明当前 bundled Codex CLI 路径没有产生 `x-oai-attestation`。它不是签名 Codex Desktop，因此不能作为 DeviceCheck E2E；该请求只用于确认“无证明”路径和观测字段。候选已停止，正式实例 SHA `9378ba6d…2020b`、PID `25190` 保持不变。

进一步用同一官方二进制启动独立 app-server，并在 `initialize` 声明 `capabilities.requestAttestation=true`。当 turn 开始时，app-server 向客户端发出 `attestation/generate` 请求；由于这个独立进程没有 Desktop provider，随后超时。该结果与官方协议一致，证明真实 token 的最后生成责任在 Desktop 宿主，Sub2API 不应自行伪造。

随后复用了 ChatGPT.app 内置的官方 `native/devicecheck.node` provider（`isSupported=true`，生成过程只在本机内存中处理，不输出 token），由官方 app-server 客户端响应 `attestation/generate`。旧候选的 118 `observe` 日志记录：

```text
present=true value_count=1 length=2987 malformed=false v=1 s=0
```

该真实证明请求在旧候选上返回 HTTP 200。随后在当前源码候选 `4093453d4`、`mode=all` 下，用同一官方 app-server + DeviceCheck provider 执行一次完整 turn：app-server 收到 1 次 `attestation/generate`，候选返回 `turn/completed`，上游请求返回 HTTP 200。证明值只在 provider、JSON-RPC 响应和当前出站请求内存中短暂存在，没有写入日志或文档；正式实例仍为 SHA `9378ba6d…2020b`、PID `25190`，候选结束后 18081 已释放。这是当前源码 HTTP 主链路的真实 DeviceCheck provider 级 E2E。

## 原生 WS 旁路结果

继续让官方 app-server 声明 `supports_websockets=true`，并用同一真实 DeviceCheck provider 在当前源码候选上尝试原生 WS。候选收到下游 WebSocket 握手（HTTP 101），app-server 共响应了 8 次 attestation 生成请求；118 日志明确显示 `openai.websocket_ingress_started`，随后当前分组的 4 个候选账号均因不支持该模型的 WS 能力被过滤，未建立上游 WS。客户端按既有策略重试后回退 HTTP，最终 `turn/completed`，正式实例仍未被切换。该结果是环境账号能力门禁，不能归因于 attestation 代码失败；原生 WS 的 scope/pool 行为仍以离线 pool 测试为主要证据，待有可用 WS 账号时再做真实上游 WS E2E。
