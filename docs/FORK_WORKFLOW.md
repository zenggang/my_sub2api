# 自有 fork 工作流

本仓库采用用户确认的三条规则：官方代码同步到 `main`；自有改造维护在 `release`，开发分支从 `release` 创建；验证后的通用改造，经过用户明确允许才向官方提 PR。

## 分支职责

| 分支或远端 | 用途 | 允许的代码来源 |
| --- | --- | --- |
| `upstream/main` | 官方主干的远端跟踪引用 | 官方 `Wei-Shaw/sub2api` |
| `main` / `origin/main` | 我们保存的官方主干 | 快进同步官方主干，不放自有改造 |
| `release` / `origin/release` | 自有改造的集成分支 | 验证通过的开发分支、明确要求合入的官方更新 |
| `feature/*`、`fix/*`、`chore/*` | 开发和验证 | 从 `release` 创建 |
| `sync/*` | 验证官方代码与自有改造的集成 | 从 `release` 创建，再合并 `main` |
| `contribute/*` | 向官方提交独立修复的整理分支 | 从 `upstream/main` 创建，只移植选定的通用提交 |

远端约定：`origin=https://github.com/zenggang/my_sub2api.git`，`upstream=https://github.com/Wei-Shaw/sub2api.git`。

## 1. 自有开发

先检查工作区和远端。开发基点必须是已经核对的 `release`；本地落后于 `origin/release` 时先快进同步。存在无关未提交工作时保留它，优先使用独立 worktree，不自动 stash、覆盖或清理。

以下示例在干净工作区执行，分支名按实际任务选择：

```bash
git fetch origin
git switch release
git merge --ff-only origin/release
git switch -c fix/example-change release
# 实现、检查差异、运行与改动相符的验证，再提交明确文件。
```

验证完成后，在已获授权的任务范围内将开发分支合入 `release`。合并前重新核对 `release` 是否变化；基线变化可能需要重新集成和测试。允许使用保留开发边界的 merge commit；不重写已推送的共享历史。

自有提交独立、范围清楚，便于以后移植到官方贡献分支。不要把运行环境配置、内网诊断与通用代码混成一个提交。

## 2. 用户要求同步主干

用户说“拉官方最新代码”“同步主干”“合并到 main/mian”时，执行以下范围：

1. 检查工作区、远端 URL、本地 `main` 与 `origin/main` 的关系。
2. 从 `upstream` 拉取官方最新 `main`，同时检查 `origin/main` 是否被其他人更新。
3. 在独立 worktree 中优先以快进方式更新本地 `main`，然后推送到 `origin/main`。干净且不影响当前任务时也可在当前工作区切换。
4. 回读确认本地 `main`、`origin/main` 与本次拉取的官方目标 SHA 一致，报告更新范围。官方若在核验期间继续前进，明确本轮同步到的 SHA。

干净工作区的等价命令为：

```bash
git fetch upstream main
git fetch origin main
git switch main
git merge --ff-only origin/main
git merge --ff-only upstream/main
git push origin main
git switch release
```

执行前先验证祖先关系。若 `main` 含自有提交、发生分叉，或 `origin/main` 无法通过官方历史解释，应报告具体差异，保留当前状态；不要强制覆盖、重置或自动把私有历史混进 main。不要直接点击会丢弃 fork 提交的强制同步操作。

**同步 main 的完成标志是源码和远端 SHA 一致。它不代表 release 已升级，也不触发部署。** 不创建后台自动同步任务，按用户的同步指令执行。

## 3. 用户要求将 main 合入 release

这是与“只同步 main”不同的操作。用户要求合入 release 时，从 `release` 创建 `sync/upstream-<标识>` 分支，在该分支合并 `main`：

```bash
git switch release
git switch -c sync/upstream-example release
git merge main
# 解决冲突，运行相关验证；检查迁移及自有补丁行为。
```

验证通过后合回 `release`。合并没有文本冲突，也需要检查功能行为；重点覆盖受影响的模型协议、HTTP/WS、工具续聊和既有私有补丁。出现数据库迁移时评审其执行影响，不能因为二进制可回滚就假定数据库也可回滚。

## 4. 经用户允许向官方提交 PR

开发仍从 `release` 开始。验证完成后，整理一份明确的贡献结果给用户：解决的问题、具体 diff、实际测试、影响范围和待确认事项。

为了避免带上其他自有改造，向官方贡献时从 `upstream/main` 创建干净的 `contribute/<主题>` 分支，只移植该修复需要的通用提交，并在这个真正的 PR 基线上重新验证。若有依赖，应明确列出，不通过把整个 release 合到该分支来引入。

得到用户对该贡献的明确允许后，才推送用于公开贡献的分支并创建指向 `Wei-Shaw/sub2api:main` 的 PR；草稿 PR 也遵守这条规则。普通开发任务、测试完成或推送我们自己的 release，都不自动授权向官方提交 PR。CLA 签署以及向官方发评论也不能代替用户自行确认。

官方 PR 不包含本 fork 的工作流规则、内网地址、账号配置、部署记录、密钥或原始设备证明。官方采纳后，通过正常 `main` 同步和 `release` 集成流程吸收，核对行为后再收敛重复补丁。

## 5. 发布边界

`release` 表示我们的集成代码线，不意味着每次提交都发布到服务器。部署和发布 tag 按用户的发布指令单独执行，并保留源码 SHA、构建 SHA、测试、候选验收和回滚记录。

仓库继承的发布 Action 监听 `v*` tag 和手动触发。普通分支推送可能启动 CI；创建 Git 分支不等于创建发布 tag。本工作流不修改这些 Action 的触发条件。

## 初始建立记录

2026-09-11，`release` 从已核对的本地 `main`（`4726bdd08b6201d426a80529b79be123a4008d20`）创建。工作流文档和根目录 `AGENTS.md` 在从 `release` 拉出的 `chore/fork-workflow` 上编写，验证后合入 `release`；`main` 保持官方代码。

建立分支不等于导入已在线运行的 Lite→5.5 私有补丁，也不等于实现设备证明适配。这两项后续分别按开发流程处理。与运行环境相关的既有分析保存在被 Git 忽略的 `docs-local/`。
