# 提交与 PR 规范

PR 汇总对应 [Issue](issue.md) 的交付和验收，具体背景、分析与执行过程仍以 Issue 为主。提交和评审前阅读 [开发规范](../guide/development.md) 与 [质量规范](../guide/quality.md)。

## 提交规则

- 一个提交只包含一个当前已实现、可独立审查和验收的原子功能；每完成一个就检查、提交一次。该功能的完整代码、适用测试、必要文档和配置应一并提交，可跨多个文件，提交描述必须与实际改动对应。其他独立功能或无关修改分别提交，不积攒多个功能后合成大提交。
- message 使用一句精简、准确的中文描述本次原子功能的实际变化，沿用 `feat:`、`fix:`、`docs:`、`chore:` 等现有前缀；不拼接多个功能说明、多条提交信息或大段改动清单。关联 Issue 时同一行附 `(#N)`，`#N` 指当前仓库，例如 `docs: 明确中文原子提交规范 (#N)`。详细背景、实施过程和验收记录写入 Issue/PR。
- 提交前检查工作区，按明确路径暂存本功能文件并检查暂存差异；不使用 `git add .` 混入他人或无关改动。
- 业务逻辑提交前必须完成 build、test、项目配置的 `golangci-lint`；不能以 `go vet` 替代 lint，不能降低规则或跳过失败检查。检查范围和命令见 [quality.md](../guide/quality.md)。
- 纯文档、注释和提交规范等不涉及业务逻辑的变更不运行 build/test/lint，检查链接、内容、差异与版本管理即可。CI 对测试、fixture、依赖、镜像和 workflow 变更的覆盖要求仍然有效。
- 默认不创建/切换分支、不推送；按用户明确授权执行相应操作。CLI/Web 独立分支与共享接口约束见 [development.md](../guide/development.md)。
- 提交 PR 时直接以正式 PR 形式创建，不使用草稿（Draft）PR。

## PR 必须包含的两部分

### 1. 内容

列出当前 PR 涉及的全部跟踪 Issue，并简述每个 Issue 对应的功能与代码/文档范围；共享接口变更说明 CLI、Web 和 internal 的影响。只完成部分内容时明确范围与剩余任务，不使用自动关闭语法提前结项。

标题与正文围绕最终交付的实际行为。PR 和辅助文档链接路线决策及 Issue，不另建冲突方案。涉及产品方案调整时，先在 Issue 记录结论并同步路线。

### 2. 验收

逐 Issue 汇总验收项、实际结果和证据，注明被测源码/制品、环境/版本、检查或 CI run。失败、阻塞、跳过和未执行均明确写出；不能把未执行写成通过。多个 Issue 共用一次验证时说明覆盖关系，不能只写一个笼统的“测试通过”。

真实外部组件的核心业务须满足 [质量规范](../guide/quality.md) 和对应路线矩阵；没有 runtime、缺必需案例或缺 CI 证据时不能放行。涉及 UI/UX 时直接链接对应 Issue 的截图或 MP4；harness/模型在 Issue 的交付记录中可追踪，PR 可直接引用。

合并前检查对应 Issue 的交付完整性和剩余门禁；发布还必须满足候选制品与支持矩阵要求。未满足 Issue 验收标准时不得将其标记完成。

## 合并门禁

GitHub 不做技术强制：本仓库是用户账号下的私有仓库，分支保护与 rulesets 需要 GitHub Pro 或公开仓库（当前返回 403），GitHub merge queue 仅面向组织仓库开放。合并门禁因此按以下流程执行，不依赖 GitHub 的强制检查。

- 合并 PR 前：该 PR head SHA 对应的 GitHub Actions `Intern quality` 运行（`push` 事件）必须已完成，`quality-gate` 与全部 job 成功；运行 URL 附在 PR 中。同一 SHA 重跑时只以最新 attempt 的完整结果为准；`container-*` 证据制品按 job 以 `overwrite: true` 替换，被取消 attempt 留下的同名制品不得参与判定，历史 attempt 的日志仍保留在运行记录中。`scope` 判定为不需要重型检查（`required=false`）时，`quality-gate` 成功只在符合下一条 `scope` 规则时才视为通过。没有对应运行或结果不完整时不得合并。
- `scope` 规则：main 的 `push` 按该提交自己的推送增量（`github.event.before` 到该提交）判定，main 上每个提交都有不被取消的独立运行；其他分支的 `push` 按整条分支相对 main 的累计差异（`git merge-base origin/main <head>` 到 head）判定，不看最后一次推送的增量，所以后续的纯文档推送不会掩盖此前被取消、失败或没跑完的代码推送；tag、定时与手动运行总是整套。判定范围内只有 `*.md` 才是 `required=false`；基点缺失或无法解析一律整套（`required=true`）。核对时看 `scope` job 日志里的 `quality-scope:` 行，它写明基点与结论。完整规则见 [质量规范](../guide/quality.md)。
- 合并后：main 上合并提交对应的 `Intern quality` 运行必须为绿色，其 URL 记录在对应 PR/Issue 中；main 变红时先修复，再合并任何后续 PR。main 上 `required=false` 的绿色运行只表示该提交相对上一个 main 提交没有代码改动，不能算作此前变红的 main 已修复。
- GitHub 侧没有强制的原因即上述计划限制。工作流不监听 `pull_request`（单 runner 串行，同仓 PR 分支的 `push` 已经产生整轮运行），也没有 `merge_group`；若仓库日后迁入符合条件的组织并启用 GitHub merge queue，需要在 `.github/workflows/quality.yml` 增加 `merge_group` 触发。

## GitHub 模板入口

默认 PR 模板位于**实际 Git 根目录**的 [.github/pull_request_template.md](../../.github/pull_request_template.md)，对应本项目目录的 `.github/pull_request_template.md`；GitHub 新建 PR 时自动带入下方“内容 / 验收”正文。

模板正文与本页“PR 正文模板”保持一致；修改其中一处时同步另一处。模板进入仓库默认分支（当前为 `main`）后才在 GitHub 新建界面生效，本地新增不代表已上线。规则依据见 [GitHub 模板说明](https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/about-issue-and-pull-request-templates)。此入口交付追踪 [IotProject#12](https://github.com/flyingEirc/IotProject/issues/12)。

## PR 正文模板

```markdown
## 1. 内容

| 跟踪 Issue | 本 PR 交付内容 | 完成范围与剩余工作 |
| --- | --- | --- |
| #N | | |

- 产品路线 / 决策依据：
- 共享接口及 CLI/Web 影响（若适用）：

## 2. 验收

| Issue / 验收项 | 实际结果 | 方法 / 环境 / 版本 | 证据（提交/制品/CI/附件） |
| --- | --- | --- | --- |
| #N / 1 | 待执行 | | |

- 本地检查：具体命令与结果；纯文档说明不适用的业务检查。
- 真实组件 / 候选制品门禁：结果或阻塞原因；不适用时说明原因。
- UI/UX 截图或 MP4：关联 Issue 的交付证据；不适用时说明原因。
- Harness / 模型：关联 Issue 的交付记录。
- 未完成项、限制与是否满足合并条件：
```
