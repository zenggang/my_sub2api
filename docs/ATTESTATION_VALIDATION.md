# `x-oai-attestation` 验证记录

本记录对应分支 `design/attestation-forwarding` 的当前实现。原始证明值、API Key 内容和 Authorization 从不写入本文件。

## 代码与离线验证

- HEAD：`e9d5bc221`（已推送到 `origin/design/attestation-forwarding`）。
- `go test ./...`：通过。
- 前端 `pnpm build`：通过。
- Linux amd64：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -trimpath`，通过。
- 本地候选二进制 SHA-256：`ef0b569e51274fc98dbeda42d9d14a81e42638e46a6f843c63496a7d13aaf0c3`。

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

本次候选与正式实例共用运行环境中的 DB/Redis；候选未切换正式服务、未替换正式二进制。启动时正式实例存在活动请求，因此没有使用 `--allow-active` 或重启正式服务；候选仅绑定 18081，验证后立即停止。正式部署仍需独立的活动请求和迁移门禁。

## 证据边界

API Key smoke 证明候选在 `all` 配置下的 HTTP Responses、模型映射、工具续聊和正式实例隔离行为；它没有产生真实 Codex Desktop 的 Apple DeviceCheck 证明，也没有证明上游接受任意伪造的 `x-oai-attestation`。真实 Desktop HTTP、compact、原生 WS 和 WS→HTTP bridge 仍需用签名 Desktop 产生的实际证明做一次受控 E2E。

正式部署和向官方提交 PR 均未执行。

## 官方 CLI 观测

使用 `/Applications/ChatGPT.app/Contents/Resources/codex`（`codex-cli 0.153.4`，本机 ChatGPT 登录状态）通过 SSH 隧道访问 118 候选 18081，候选设置为 `mode=observe`。请求返回 HTTP 200；候选 Info 日志记录：

```text
openai_attestation_observed transport=http target=codex present=false value_count=0
```

这证明当前 bundled Codex CLI 路径没有产生 `x-oai-attestation`。它不是签名 Codex Desktop，因此不能作为 DeviceCheck E2E；该请求只用于确认“无证明”路径和观测字段。候选已停止，正式实例 SHA `9378ba6d…2020b`、PID `25190` 保持不变。

进一步用同一官方二进制启动独立 app-server，并在 `initialize` 声明 `capabilities.requestAttestation=true`。当 turn 开始时，app-server 向客户端发出 `attestation/generate` 请求；由于这个独立进程没有 Desktop provider，随后超时。该结果与官方协议一致，证明真实 token 的最后生成责任在 Desktop 宿主，Sub2API 不应自行伪造。

随后复用了 ChatGPT.app 内置的官方 `native/devicecheck.node` provider（`isSupported=true`，生成过程只在本机内存中处理，不输出 token），由官方 app-server 客户端响应 `attestation/generate`。118 候选 `observe` 日志记录：

```text
present=true value_count=1 length=2987 malformed=false v=1 s=0
```

该真实证明请求在候选上返回 HTTP 200。随后将候选切为 `mode=all`，用同一官方 app-server + DeviceCheck provider 再执行一次，turn 状态为 `completed`，上游请求返回 HTTP 200。候选二进制 SHA-256 为 `8a1211943255380b53bf503344915ace811bd3a1c0453032b688ad4340d1e706`；正式实例仍为 SHA `9378ba6d…2020b`、PID `25190`。这是 HTTP 主链路的真实 DeviceCheck provider 级 E2E；没有把 token 写入日志或文档。
