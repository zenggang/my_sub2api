# opencode-capacity-retry

2026-10-26：替身按当天观测到的 `ttft_admission_refused` 与参数错误响应编写。CLI 未登录，未检索所有者工作区 mock；不上传或发布。

## Purpose

唯一成员 OpenCode upstream 为真实网关 handler、调度、限流处理及仓储 HTTP 传输提供容量拒绝和成功响应，检查等待、同账号恢复、耗尽后切号与取消。

## Execution

```sh
postman simulation run postman/simulations/opencode-capacity-retry.sim.yaml --port 4907 --no-history --output ndjson
cd backend
TEST_OPENCODE_CAPACITY_SIMULATION_URL=http://127.0.0.1:4907/opencode go test -tags=unit,simulation ./internal/handler -run '^TestOpenCodeCapacityPostmanSimulation$' -count=1
```

测试通过 `/_requests` 验证调用账号、次数与实际等待；检查模拟器 request 事件，结束后 Ctrl-C 停止。每轮需新启模拟器以清空记录。

## Assumptions

- 2026-10-26：账号和配额快照为合成数据，HTTP 传输使用正式仓储实现；不访问生产，不验证真实上游容量恢复概率。

## Observations

<!-- begin:observations -->
2026-10-26：唯一成员 OpenCode upstream 实际承接全部 HTTP 流量，8 个场景通过：三个入站协议与流式请求等待约 2 秒后同账号恢复；首次加三次重试后才切备用账号；单账号耗尽返回 429；15 秒等待可在约 300ms 取消；参数 400 仅请求一次。配额和临时停调写入均为零。未验证真实上游恢复概率。
<!-- end:observations -->

## 2026-11-07 官方 0.2.20 合并回归

集成源码 `e5789fedfce6db6007fa0cee0a284bb675397ca8` 使用 CLI 1.73.0 和既有本地替身复验，8 个场景全部通过；模拟器记录 27 个实际 HTTP request 事件，测试同时核对采集接口的账号、次数、等待与取消结果。复用原有替身，没有上传或发布资源。该证据仅支持这些合成容量拒绝场景下的客户端处理，不证明真实上游容量或恢复概率。
