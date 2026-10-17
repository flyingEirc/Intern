# 提交与 PR 规范

PR 汇总对应 [Issue](issue.md) 的交付和验收，具体背景、分析与执行过程仍以 Issue 为主。提交和评审前阅读 [开发规范](../guide/development.md) 与 [质量规范](../guide/quality.md)。

## 提交规则

- 每完成一个独立小功能或小业务就检查、提交一次；一个提交只解决一个明确问题，包含相关实现与必要测试，不积攒多个业务后大提交。
- message 用一句简短概括，沿用 `feat:`、`fix:`、`docs:`、`chore:` 等现有前缀。关联 Issue 时同一行附 `(#N)`，例如 `docs: 整理文档阅读索引 (#11)`，不堆叠长说明。
- 提交前检查工作区，按明确路径暂存本功能文件并检查暂存差异；不使用 `git add .` 混入他人或无关改动。
- 业务逻辑提交前必须完成 build、test、项目配置的 `golangci-lint`；不能以 `go vet` 替代 lint，不能降低规则或跳过失败检查。检查范围和命令见 [quality.md](../guide/quality.md)。
- 纯文档、注释和提交规范等不涉及业务逻辑的变更不运行 build/test/lint，检查链接、内容、差异与版本管理即可。CI 对测试、fixture、依赖、镜像和 workflow 变更的覆盖要求仍然有效。
- 默认不创建/切换分支、不推送；按用户明确授权执行相应操作。CLI/Web 独立分支与共享接口约束见 [development.md](../guide/development.md)。

## PR 必须包含的两部分

### 1. 内容

列出当前 PR 涉及的全部跟踪 Issue，并简述每个 Issue 对应的功能与代码/文档范围；共享接口变更说明 CLI、Web 和 internal 的影响。只完成部分内容时明确范围与剩余任务，不使用自动关闭语法提前结项。

标题与正文围绕最终交付的实际行为。PR 和辅助文档链接路线决策及 Issue，不另建冲突方案。涉及产品方案调整时，先在 Issue 记录结论并同步路线。

### 2. 验收

逐 Issue 汇总验收项、实际结果和证据，注明被测源码/制品、环境/版本、检查或 CI run。失败、阻塞、跳过和未执行均明确写出；不能把未执行写成通过。多个 Issue 共用一次验证时说明覆盖关系，不能只写一个笼统的“测试通过”。

真实外部组件的核心业务须满足 [质量规范](../guide/quality.md) 和对应路线矩阵；没有 runtime、缺必需案例或缺 CI 证据时不能放行。涉及 UI/UX 时直接链接对应 Issue 的截图或 MP4；harness/模型在 Issue 的交付记录中可追踪，PR 可直接引用。

合并前检查对应 Issue 的交付完整性和剩余门禁；发布还必须满足候选制品与支持矩阵要求。未满足 Issue 验收标准时不得将其标记完成。

## GitHub 模板入口

默认 PR 模板位于**实际 Git 根目录**的 [.github/pull_request_template.md](../../../.github/pull_request_template.md)，对应本项目目录的 `../.github/pull_request_template.md`；GitHub 新建 PR 时自动带入下方“内容 / 验收”正文。

模板正文与本页“PR 正文模板”保持一致；修改其中一处时同步另一处。模板进入仓库默认分支（当前为 `cli/container-quality-gate`）后才在 GitHub 新建界面生效，本地新增不代表已上线。规则依据见 [GitHub 模板说明](https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/about-issue-and-pull-request-templates)。此入口交付追踪 [#12](https://github.com/flyingEirc/IotProject/issues/12)。

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
