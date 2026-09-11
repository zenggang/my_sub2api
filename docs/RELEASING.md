# 编译、打包与部署操作手册

依据已执行过的 Lite→5.5 发布、候选演练及当次读取的发布脚本整理。日常按本文执行；只重新核实会变化的源码、工具兼容性、迁移、线上 SHA 和活动请求。分支规则见 [FORK_WORKFLOW.md](FORK_WORKFLOW.md)。

本文区分“已有工具实际支持的操作”和“尚未接入的源码来源”，不以编译成功、服务 active 或 health 200 代替完整发布验收。

## 0. 固定入口与当前支持范围

具体机器、目录和版本值从本地 `docs-local/DEPLOYMENT_PROFILE.md` 取得。该文件由 Git 忽略，只保存非敏感定位信息；凭据仍留在运行环境。

| 操作 | 固定入口 | 实际边界 |
| --- | --- | --- |
| 查看官方版本 | `check-118.sh` / `prepare.py check` | 只检查，不部署 |
| 导出源码并应用补丁 | `prepare.py prepare TAG --repo CACHE --output OUTPUT` | 官方 tag＋固定 Lite 补丁 |
| 编译及定向回归 | `prepare.py build PREPARED` | 前端、原生 Go 测试、Linux amd64 二进制 |
| 上传候选 | `prepare.py stage PREPARED` | 创建新候选目录并上传六份材料，不切换服务 |
| 服务器预检 | `sub2api-patched-release --check DIR LIVE_SHA` | 校验版本、SHA、manifest、迁移和基础健康 |
| 候选端到端验证 | `canary.sh DIR TEST_KEY_ID` | 独立监听地址，共用线上 DB/Redis，产生真实请求 |
| 正式部署 | `sub2api-patched-release --deploy DIR LIVE_SHA` | 保存备份、活动请求检查、原子替换、重启和回查 |
| 二进制回滚 | `sub2api-patched-release --rollback BACKUP LIVE_SHA` | 恢复兼容备份；不回滚数据库 |

**当前工具不是任意 fork commit 的发布器。** `prepare.py` 固定读取 `lite55.patch`，版本来自官方 tag＋`patch_id`；远程入口只接受 `-lite55.1` / `-lite55.2`。在线 prepare 会拉官方 tag，`--ref` 仅在 offline 模式使用，不能借它假称正在打包 fork HEAD。

日后从 `release` 发布新增改造，先在独立开发任务中将构建来源、版本规则、manifest 和具名测试接入该提交并验证。不要在一次部署过程中临时修改校验器、换版本名或清空迁移差异来强行放行。导入工具或扩展发布器属于代码维护；本文不会把尚未完成的接入写成已支持。

## 1. 编译方式

### 1.1 环境与源码冻结

本机曾验证的 Go 为 `1.27.1 darwin/arm64`，目标源码要求 Go `1.27.0`。Node 为 `20.20.2`。pnpm、Python 的实际路径与版本见本地档案；每次构建把版本保存到 build log，不假定全局 PATH 或官方 CI 的版本等于本机版本。

先设定本次 `SUB2API_TOOLKIT`、`GO_BIN`、`PNPM_BIN`、`SUB2API_TAG`、`SUB2API_SOURCE_CACHE`、`SUB2API_BUILD_ROOT`。值使用本地档案，不输入密钥。

```bash
cd "${SUB2API_TOOLKIT:?set toolkit directory}"
python3 prepare.py prepare "${SUB2API_TAG:?set reviewed official tag}" \
  --repo "${SUB2API_SOURCE_CACHE:?set persistent source cache}" \
  --output "${SUB2API_BUILD_ROOT:?set persistent artifact directory}"
```

记录工具输出的 `PREPARED=...`，赋给 `SUB2API_PREPARED`。prepare 执行固定补丁 SHA 检查、干净源码导出、`git apply --check`、应用补丁、差异校验并生成 manifest。源码位于 `PREPARED/source`；不能继续在里面随手编辑后复用旧构建证明。

检查 manifest 的 `official_commit`、`patch_sha256`、`source_tree`、`migration_changes`。迁移比较使用登记的官方基线；发布前还要确认它确实对应当前运行基线。过期 baseline 不能替代现场核对。

### 1.2 标准编译命令

在 Bash 中执行，确保经过 `tee` 后仍保留构建失败：

```bash
set -o pipefail
cd "${SUB2API_TOOLKIT:?}"
(
  set -e
  unset GOROOT GOOS GOARCH
  "${GO_BIN:?}" version
  node --version
  "${PNPM_BIN:?}" --version
  python3 --version
  python3 prepare.py build "${SUB2API_PREPARED:?}"
) 2>&1 | tee "$SUB2API_PREPARED/build.log"
```

`GO_BIN` 和 `PNPM_BIN` 必须 export，供 Python 子进程使用。复用持久工具缓存，不每次下载编译器。

build 的固定顺序：

1. 再核补丁 SHA、反向应用检查、源码工作区无改动及 `source_tree` 一致。
2. 在 `source/frontend` 执行 `pnpm install --frozen-lockfile`、`pnpm build`。build 包括 i18n 检查、`vue-tsc -b` 和 Vite；输出进入 `backend/internal/web/dist`。
3. 清除继承的 `GOROOT/GOOS/GOARCH`，在 Mac 本机运行定向 Go 回归，保存 `tests.jsonl`。交叉编译变量只用于下一步，不能让 Mac 尝试运行 Linux 测试二进制。
4. 使用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`，配合 `-tags embed -trimpath` 编译 `./cmd/server`。内嵌前端必须先存在；普通 `backend make build` 没有 `-tags embed`，不是这里的发布命令。
5. 生成二进制 SHA、应用差异和测试记录；成功后 manifest 才是 `BUILT_TESTED`。

对应底层 Go 命令如下，仅用于理解/诊断既有 build，不是让日常再编译一次，也不会单独生成通过的 manifest：

```bash
cd "${SUB2API_PREPARED:?}/source/backend"
env -u GOROOT -u GOOS -u GOARCH \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "${GO_BIN:?}" build \
  -tags embed -trimpath \
  -ldflags "-s -w -X main.Version=${SUB2API_BUILD_VERSION:?} -X main.Commit=${SUB2API_BUILD_COMMIT:?} -X main.BuildType=release" \
  -o "$SUB2API_PREPARED/sub2api" ./cmd/server
```

现有 wrapper 的 `main.Commit` 是官方 SHA＋`+lite55`，实际来源还需结合 manifest 的源码树和补丁 SHA。Go 中时间变量名是 `main.Date`，不是 `main.BuildTime`；当前 wrapper 未设置 Date。不要借修改时间或版本字段伪装同一构建；相同源码重建也未必得到相同二进制 SHA，每个候选单独验证。

当前必要回归为 `TestMappedGPT55*`、`TestOpenAIGatewayServiceForward_NormalizesResponsesLiteToolsForOAuth`、`TestProxyOpenAIWSHTTPBridgeTurn*`。wrapper 还检查两个必要测试名称确实出现在通过事件中。退出码 0 但未跑到目标测试不是通过。新增功能需要扩展对应测试，旧 Lite 用例不能证明新功能正确。

2026-09-11 在 fork 集成 lite55.2 时确认：`openai_responses_lite_tools_test.go` 带 `//go:build unit`，现有旧 wrapper 的无标签测试不执行其中的用例。对 fork 的代码集成，必须另外执行下面的定向回归，并核对目标测试名称实际出现。命令在本 fork 根目录运行，GO_BIN 使用部署档案中的已 export 路径：

```bash
(
  set -e
  cd backend
  env -u GOROOT -u GOOS -u GOARCH "${GO_BIN:?}" test -tags=unit ./internal/service \
    -run 'TestMappedGPT55|TestNormalizeOpenAIResponsesLite|TestApplyCodexOAuthTransform_PreservesLiteNamespaceToolChoice|TestOpenAIGatewayServiceForward_.*ResponsesLite|TestOpenAIBuildUpstreamRequestOpenAIPassthroughForwardsResponsesLiteHeader|TestProxyOpenAIWSHTTPBridgeTurn' \
    -count=1 -json
)
```

该步骤验证 fork 代码，不改变旧 wrapper 的源码来源，也不能把由旧 wrapper 生成的官方 tag＋补丁二进制标为 fork HEAD 的发布包。

## 2. 打包方式

编译成功后的候选目录是部署单位，禁止只拿一个不明来源的 `sub2api` 文件替换线上。

| 文件 | 用途 |
| --- | --- |
| `sub2api` | Linux amd64 发布二进制，包含前端 |
| `manifest.json` | 官方源码、补丁、版本、源码树、二进制及差异 SHA、测试状态、迁移差异 |
| `lite55.patch` / `applied.patch` | 固定输入补丁与实际应用结果 |
| `baseline.json` | 本次准备时的基线快照 |
| `tests.jsonl` | 实际执行的具名 Go 测试事件 |
| `build.log` | 本次工具链版本、前端与 Go 构建输出 |
| `source.tar.gz` | 构建前冻结并提交到准备目录的源码归档，不包含 node_modules/凭据 |
| `SHA256SUMS` | 分发材料的文件校验和 |

已有 stage 固定上传 `sub2api`、`manifest.json`、`lite55.patch`、`applied.patch`、`baseline.json`、`tests.jsonl` 六个文件。以下命令额外生成固定格式的归档包，用于保存、交付与后续查证；不改变 stage 或服务器的安装协议：

```bash
(
  set -e
  cd "${SUB2API_PREPARED:?}"
  jq -e '.status == "BUILT_TESTED" and .tests_passed > 0' manifest.json >/dev/null
  test -s build.log
  git -C source diff --quiet
  test -z "$(git -C source status --porcelain)"
  test "$(git -C source rev-parse 'HEAD^{tree}')" = "$(jq -r .source_tree manifest.json)"
  test "$(shasum -a 256 sub2api | awk '{print $1}')" = "$(jq -r .binary_sha256 manifest.json)"
  git -C source archive --format=tar.gz --output="$SUB2API_PREPARED/source.tar.gz" HEAD
  shasum -a 256 sub2api manifest.json lite55.patch applied.patch baseline.json tests.jsonl build.log source.tar.gz > SHA256SUMS
  shasum -a 256 -c SHA256SUMS
  tar -czf "$SUB2API_PREPARED/release-linux-amd64.tar.gz" \
    sub2api manifest.json lite55.patch applied.patch baseline.json tests.jsonl build.log source.tar.gz SHA256SUMS
  tar -tzf "$SUB2API_PREPARED/release-linux-amd64.tar.gz"
  shasum -a 256 "$SUB2API_PREPARED/release-linux-amd64.tar.gz"
)
```

`SUB2API_PREPARED` 使用绝对路径。保存归档包 SHA；验收时在新目录解包并校验 `SHA256SUMS`。归档命令根据现有产物格式固化，本轮只作语法核验，未重新编译或生成生产包。不要把 tar 直接解压到正式安装目录。

## 3. 上传和候选验证方式

```bash
cd "${SUB2API_TOOLKIT:?}"
python3 prepare.py stage "${SUB2API_PREPARED:?}"
```

记录 `STAGED=...` 的实际目录。stage 使用 SSH/SCP，新目录已存在则拒绝；不能靠覆盖旧文件来复用旧候选。上传后在服务器执行 `file`、版本命令和 SHA 检查，以服务器校验 Linux ELF。Mac 上不要执行 Linux 二进制。

服务器的 `--check`、`canary.sh` 精确命令见本地部署档案。canary 必须在数据库迁移预检之后运行：它共用线上 DB/Redis，启动本身就可能执行迁移。指定已授权且有模型权限的测试 Key ID；不要把 Key 明文放在命令或文档里。

现有 canary 覆盖 5.5、Sol、Terra、Luna、Astra 五模型，每个模型先触发 namespace 工具调用，再提交工具结果续聊，共十轮；每轮要求 `response.completed`，保持下游请求模型名，续聊最终输出 `hello`。结合这些请求的日志/usage 再核实际上游模型：原生调用通过不等于模型映射通过。

只有十轮都通过、候选 SHA 不变且正式二进制 SHA 不变，工具才写 `SMOKE_VERIFIED.sha256`。结束时停止自己创建的候选 unit；端口被占用时拒绝，不停止无关进程。新验证失败会让旧通过标记失效，不能手工补一个标记。

## 4. 正式部署方式

用户已授权发布后，使用档案中的固定 `--deploy DIR LIVE_SHA` 入口。部署前重新读取 live SHA，不长期复用旧值；确认账号配置、当前活动请求、候选身份、迁移基线和候选通过标记。

入口依次执行：排他锁 → 当前二进制与候选校验 → 冒烟 SHA 检查 → Guard 活动请求和账号并发检查 → 备份 → 原子替换二进制 → 重启 Sub2API → 内外健康检查、启动错误筛查、配置及映射前后比对。

备份至少包含旧二进制及 SHA、运行配置、环境文件、映射快照。备份可能含凭据，仅保存在受限目录，不上传 Git。更新 Sub2API 只操作该组件；不顺带发布或重启 Guard、Console、Quick Ops。

`active_requests` 非零时默认退出，等下一次检查；不改账号 schedulable 来制造空闲。`--allow-active` 只有用户明确允许中断活动请求时才可使用。当前工具是切换前检查，不能保证检查与重启之间不会有新请求，因此不要承诺严格零中断。

部署失败时入口自动恢复旧二进制并复查健康。手动 `--rollback` 只支持该工具生成的兼容备份；当前入口在 rollback 前也要求服务基础健康，已经停服时使用本地档案里的应急恢复步骤，不能反复执行必然被同一前置条件拒绝的命令。

部署后必须补业务验收：对正式入口执行与改动相关的真实请求，记录请求 ID、终态、实际账号/上游模型及回查窗口内新增错误。`INSTALLED`、health 200、HTTP 200 都不是业务完成证明。503 overload、HTTP/2 中断与 Lite 不兼容分别归因，按 request 去重。

## 5. 固定故障处理

| 现象 | 已确认的处理方法 |
| --- | --- |
| Go 标准库异常、版本混乱 | 清除旧 `GOROOT/GOOS/GOARCH`，显式使用档案内 GO_BIN |
| 前端缺失或页面版本旧 | 前端先构建到内嵌目录，后端使用 `-tags embed`；不能只用默认 make build |
| Mac 提示 exec format error | Linux 二进制只在目标 Linux 校验；Go 测试先在本机原生环境运行 |
| pnpm frozen-lockfile 失败 | 核锁文件和已登记 pnpm 版本，不删除锁或用非 frozen 安装临时重算依赖 |
| 补丁冲突或准备后源码变化 | 保留失败目录；在开发分支维护新补丁/源码，再重新 prepare，不吞 reject |
| 具名测试没运行 | 检查测试过滤器、build tags 和必要名称，不只看退出码或汇总数量 |
| 新迁移或登记基线不匹配 | 先完成迁移及备份/恢复评审；共用线上 DB 的 canary 也不能先启动 |
| stale_live_sha / stale_smoke_evidence | 回读实际版本和 SHA；重新验证相应候选，不关闭 SHA 门禁 |
| 候选端口占用 | 确认占用者，保留无关服务；不临时 kill 进程 |
| canary 401 / 模型无权限 | 核指定测试 Key ID、分组和模型权限，不临时改整批生产账号映射 |
| 远端中文 JSON 编码失败 | 明确解释器与编码，使用 ASCII-safe/UTF-8；先核变更是否发生再重试 |
| 远端无 rg/python3 或旧 systemctl | 用档案确认的 grep、`/usr/bin/python`；解析 `systemctl show -p MainPID` |
| 官方 updater 覆盖私有包 | 私有版本不用官方 update/管理页升级；旧 updater 仅 check-only，timer 保持禁用 |
| 回滚文件格式不兼容 | 校验真实备份内容及来源，不把另一套脚本的备份路径直接传给当前 CLI |

## 6. 发布记录

每次从 [RELEASE_RECORD_TEMPLATE.md](RELEASE_RECORD_TEMPLATE.md) 建立记录，现场信息放 `docs-local/` 或受限发布材料目录。记录本次运行的证据，不照抄历史通过结果。流程或工具变更时同步更新本文和本地档案；一般发布只新增执行记录，不重写整套方案。
