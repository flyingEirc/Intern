# Intern 核心文档目录

`docs/` 保存项目核心约束；[AGENTS.md](../AGENTS.md) 与 [CLAUDE.md](../CLAUDE.md) 提供任务阅读索引。[产品 README](../README.md) 面向使用者，帮助理解产品和上手。

| 目录 / 文档 | 内容与阅读时机 |
| --- | --- |
| [roadmap/数据库备份与恢复-2026-10-17.md](roadmap/数据库备份与恢复-2026-10-17.md) | 当前产品路线与 PRD：范围、选型、逻辑/物理备份设计、当前 CLI 交付、历史验证和后续门禁。开发前阅读。 |
| [workflow/issue.md](workflow/issue.md) | 以 GitHub Issue 跟踪背景、功能、步骤、验收、交付；含 harness/模型和 UI/UX 证据要求。开始或调整任务时阅读。 |
| [workflow/pr.md](workflow/pr.md) | 小功能提交、提交前检查、PR 的关联 Issue 与逐项验收汇总。提交或提 PR 前阅读。 |
| [guide/development.md](guide/development.md) | 模块职责、组件复用、实现和安全约束、工作区协作、开发命令。修改代码时阅读。 |
| [guide/quality.md](guide/quality.md) | 本地与 CI 检查范围、真实组件门禁、证据和支持声明边界。测试与验收时阅读。 |
| [guide/documentation.md](guide/documentation.md) | 文档职责、路线命名、变更与冲突处理、链接和文档交付检查。修改文档时阅读。 |

## 维护分工

- `roadmap/`：描述产品功能的路线、目标、依赖、设计决策和验收边界。每个 Markdown 文档使用 `产品功能名称-YYYY-MM-DD.md`，例如 `数据库备份与恢复-2026-10-17.md`。日期不代替状态或版本依据。
- `workflow/`：描述怎么跟踪任务、提交与评审；具体执行进展写入 [GitHub Issues](https://github.com/flyingEirc/IotProject/issues)。
- `guide/`：描述开发中必须遵守的规范；不要另立相互冲突的产品方案。

新增核心文档时更新本目录；新增必读约束时同步两个 AI 阅读入口。此次结构调整追踪 [#11](https://github.com/flyingEirc/IotProject/issues/11)。
