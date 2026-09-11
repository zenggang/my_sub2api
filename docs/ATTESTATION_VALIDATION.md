# `x-oai-attestation` 验证记录

本记录对应分支 `design/attestation-forwarding` 的当前实现。原始证明值、API Key 内容和 Authorization 从不写入本文件。

## 代码与离线验证

- HEAD：`2f653d5e4`（已推送到 `origin/design/attestation-forwarding`）。
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
