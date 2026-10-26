# openai-account-routing

2026-10-26：按现有 Codex 探针的 SSE 消费约定编写本地替身。Postman CLI 未登录，未执行所有者工作区 mock 检索；未上传或发布。

## Purpose

`openai-account-routing` 替代 Codex 上游，用于检查开启、关闭和错误类型配置经过真实健康/智商探针与 HTTP 传输后的请求头。响应和采集字段见 `postman/mocks/openai-account-routing/default.js`。

## Execution

```sh
postman simulation run postman/simulations/openai-account-routing.sim.yaml --port 4906 --no-history --output ndjson
cd backend
TEST_OPENAI_ACCOUNT_ROUTING_SIMULATION_URL=http://127.0.0.1:4906/codex go test -tags=unit,simulation ./internal/repository -run '^TestOpenAIAccountRoutingOverridePostmanSimulation$' -count=1
```

检查模拟器 request 事件及测试结束时的 `/_requests` 断言，完成后用 Ctrl-C 停止模拟器。

## Assumptions

- 2026-10-26：只有依赖地址在测试适配器内替换为本地地址；正式探针、账号读取逻辑、仓储传输和 SSE 解析运行真实代码。测试不模拟真实上游路由决策、负载或错误率。
- 2026-10-26：每轮模拟器新启以清空采集记录，测试期望固定16条探针请求；同一实例重复测试前需重启。

## Observations

<!-- begin:observations -->
2026-10-26：唯一成员 codex 接收全部健康/智商探针流量；OAuth/setup-token 的缺省、开启、关闭、错误类型均通过真实传输断言。额外 GET 采集接口验证请求头与每种状态的调用数。
<!-- end:observations -->

## What didn't work

- 2026-10-26：CLI 1.73.0 的成员 path 指向目录时返回 `Mock 'file' must point to a file`；此处固定指向 `config.yaml`。
