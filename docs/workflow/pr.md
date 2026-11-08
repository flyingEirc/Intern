# 提交与 PR 规范

PR 汇总对应 [Issue](issue.md) 的交付和验收，具体背景、分析与执行过程仍以 Issue 为主。提交和评审前阅读 [开发规范](../guide/development.md) 与 [质量规范](../guide/quality.md)。

## 提交规则

- 一个提交只包含一个当前已实现、可独立审查和验收的原子功能；每完成一个就检查、提交一次。该功能的完整代码、适用测试、必要文档和配置应一并提交，可跨多个文件，提交描述必须与实际改动对应。其他独立功能或无关修改分别提交，不积攒多个功能后合成大提交。
- message 使用一句精简、准确的中文描述本次原子功能的实际变化，沿用 `feat:`、`fix:`、`docs:`、`chore:` 等现有前缀；不拼接多个功能说明、多条提交信息或大段改动清单。关联 Issue 时同一行附 `(#N)`，`#N` 指当前仓库，例如 `docs: 明确中文原子提交规范 (#N)`。详细背景、实施过程和验收记录写入 Issue/PR。
- 提交前检查工作区，按明确路径暂存本功能文件并检查暂存差异；不使用 `git add .` 混入他人或无关改动。
- 业务逻辑提交前必须完成 build、test、项目配置的 `golangci-lint`；不能以 `go vet` 替代 lint，不能降低规则或跳过失败检查。检查范围和命令见 [quality.md](../guide/quality.md)。
- 纯文档、注释和提交规范等不涉及业务逻辑的变更不运行 build/test/lint，检查链接、内容、差异与版本管理即可。`.github/**`（workflow、脚本、自测与模板）属于 CI 自身文件，同样不触发 CI；改动它们时必须在本地跑通全部 `.github/scripts/*-cases.sh` 自测与 actionlint，并把输出记录在 PR（见「合并门禁」）。CI 对测试、fixture、依赖与镜像变更的覆盖要求仍然有效。
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

- 合并 PR 前：在 PR 分支上用 `workflow_dispatch` 对 head 手动触发一次 `Intern quality`（`gh workflow run quality.yml --ref <分支>`；需要不论改动范围跑全部容器层时加 `-f full=true`）。该运行的 head SHA 必须等于 PR head SHA，运行已完成，`quality-gate` 与 `scope` 选中的全部 job 成功；运行 URL 附在 PR 中。之后再推送新提交，旧的手动运行就不再对应 head，需要对新 head 重新触发。分支 `push` 运行只跑 quality-fast（`quality-gate` 日志写 `fast checks only, no container tier ran (not merge-gate evidence)`），用于开发中的快速反馈，不能作为合并证据。手动运行总会被创建（path filter 与 `[skip ci]` 都只作用于 `push`），所以合并前总有一个对应 head 的运行，不再需要按更早提交推断。手动运行按整条分支相对 main 的累计差异选层（规则见下一条）：只有 CLI 层改动时跑 quality-fast、`container-mysql` 矩阵与汇总门禁；只有 Web 层改动时跑 quality-fast 与九个 Web 容器 job；共享路径或两层都有改动时全部运行。整条 PR 相对 main 合并基点只改文档或 CI 文件时，三层全部跳过，`quality-gate` 输出 `not-required (no business change)`；此时 PR 涉及的 CI 脚本必须先在本地通过自测（`for t in .github/scripts/*-cases.sh; do "$t"; done`；`cli-quality-stop-test-cases.sh` 需要 Linux 的 `setsid`）和 `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 .github/workflows/quality.yml`，把每个脚本的 `N cases, 0 failed` 与 actionlint 结果记录在 PR；CI 自身脚本只在下一次业务运行或每日定时运行时执行。手动触发从该提交的脚本执行，重跑旧运行执行的仍是旧脚本，不能替代。同一 SHA 重跑时只以最新 attempt 的完整结果为准，且最新 attempt 必须重跑 `quality-gate`，CLI 层运行时还要汇总全部配置（只重跑单个容器腿时汇总 job 一并重跑）；`container-*` 证据制品按 job 以 `overwrite: true` 替换，被取消 attempt 留下的同名制品不得参与判定，历史 attempt 的日志与结论仍保留在运行记录中，不得删除该运行或改写结论。重跑只用于已在 PR/Issue 定位并记录的非产品 flake（runner/基础设施抖动或被外部取消）；产品或测试代码导致的失败必须先修复再重跑，一次重跑不能删除原失败证据，不用无限重试把红色洗绿。没有对应 head 的手动运行或结果不完整时不得合并。
- `scope` 规则：`on.push.paths` 决定 `push` 是否创建运行，`scope` 决定已创建的运行跑哪些层：quality-fast（fast）、CLI 容器矩阵与汇总门禁（cli）、九个 Web 容器 job（web）。分类只有一份，即 `.github/scripts/needs-ci.sh`：退出 0 表示没有业务改动；退出 1 时输出 `cli|web|all <路径>`。`app/cli/**` 是 CLI 层，`app/web/**` 是 Web 层，两者各自目录下的 Markdown 都算在内；`app/cli/go.mod`、`app/cli/go.sum`、`app/web/go.mod`、`app/web/go.sum` 与 `app/cli/internal/qualitygate/contract.json` 会被另一层读取（两个 Dockerfile 都复制全部 `go.mod`/`go.sum`，Web 的 physical 与 W3 场景读取 contract.json），算共享；`internal/**`、根目录 `Dockerfile`（Web physical 场景也构建它的 client-tools 阶段）、`.dockerignore`、`THIRD_PARTY_NOTICES`、`mise.toml`、`.golangci.yml`、`.githooks/**`、根目录 `go.*` 与其他未证明只属于一层的路径也都算共享（`all`）。各事件的规则：main 的 `push` 按这次推送的增量（`github.event.before` 到该提交）判定，有业务改动就三层全跑，不论改动属于哪一层，main 上每个提交都有不被取消的独立运行；其他分支的 `push` 按整条分支相对 main 的累计差异（`git merge-base origin/main <head>` 到 head）判定，有业务改动也只跑 fast；分支上的 `workflow_dispatch` 用同样的累计差异，按层选择，`full=true` 时三层全跑；main 上的 `workflow_dispatch`、tag 与定时运行总是三层全跑。按累计差异而不是最后一次推送的增量判定，所以被取消、失败或没跑完的代码推送不会因后续只改文档或 CI 文件的提交而被掩盖。`quality-gate` 核对选中层的每个 job 都是 `success`、未选中层的每个 job 都是 `skipped`。基点缺失或无法解析时取该事件最宽的结论：分支 `push` 跑 fast，其余三层全跑。GitHub 不评估 tag 推送的 path filter，无法生成差异（超过 1000 个提交或差异超时）时总是运行，差异超过 3000 个文件时只检查前 3000 个（filter 可能漏掉其后的业务改动），所以“有没有 push 运行”不能当作不需要 CI 的证据。核对时看 `scope` job 日志里的 `quality-scope:` 行（写明基点、触发判定的路径与层）和 `quality-gate` 的 `tiers fast=… cli=… web=…` 行。完整规则见 [质量规范](../guide/quality.md)。
- `[skip ci]`：`push` 的 head 提交信息含 `[skip ci]`、`[ci skip]`、`[no ci]`、`[skip actions]`、`[actions skip]` 或 `skip-checks: true` trailer 时，GitHub 不为该推送创建运行（[GitHub 文档](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/skip-workflow-runs)）；`workflow_dispatch` 与定时运行不受影响。只用于分支上不需要快速反馈的中间提交。进入 main 的提交信息不得含跳过指令：squash 合并默认会汇总各提交的信息，合并前要删掉其中的跳过指令；否则 main 的合并提交没有运行，按下一条用 `workflow_dispatch` 补跑。
- 开发期不等待 CI（2026-11-06 用户决定，[#19](https://github.com/flyingEirc/Intern/issues/19)）：后续开发先提交 PR、不触发 CI——分支上的提交信息带 `[skip ci]`，推送不创建运行；需要 CI 证据时对 PR head 用 `workflow_dispatch` 提交一次运行，排队等待 runner，开发与后续 Issue 不等待其结果继续推进。这只改变开发节奏：上一条的合并前条件不变，未完成或未全绿的运行不能作为合并证据；Issue/PR 中把 CI 部分记为“已提交、未执行/未完成”，不得写成通过。
- 合并后：main 上合并提交对应的 `Intern quality` 运行必须为绿色，其 URL 记录在对应 PR/Issue 中；有业务改动时这次运行三层全跑，补齐合并前按层跳过的容器 job。合并提交没有运行时（相对所依据运行的提交只改文档或 CI 文件），沿用此前最近一个 main 运行的绿色结论，并在 PR 中记录该运行 URL 与 `needs-ci.sh <该运行对应的提交> <merge 提交>` 的输出；该命令退出 1 说明合并本身含业务改动而没有运行（GitHub 计算 changed files 的盲区，或提交信息带了跳过指令），必须对 `main` 上的该合并提交用 `workflow_dispatch` 补跑并以补跑运行判定，不能沿用更早的绿色运行。main 变红时先修复，再合并任何后续 PR。main 上三层全部跳过（`not-required`）的绿色运行只表示该提交相对 `needs-ci.sh` 的基点没有业务改动，不能算作此前变红的 main 已修复。
- PR 与提交上还可能显示一条失败的 `CircleCI Pipeline` 状态（`No configuration was found`）：CircleCI 已退役，但仓库的 CircleCI webhook 尚未移除。它不是合并门禁，判定只以 `Intern quality` 运行的 `quality-gate` 与全部 job 为准；移除该 webhook（或在 CircleCI 停止构建该项目）属于仓库所有者操作，尚未执行。
- GitHub 侧没有强制的原因即上述计划限制。工作流不监听 `pull_request`（单 runner 串行，合并证据由 PR head 上的 `workflow_dispatch` 运行提供），也没有 `merge_group`；若仓库日后迁入符合条件的组织并启用 GitHub merge queue，需要在 `.github/workflows/quality.yml` 增加 `merge_group` 触发。

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
