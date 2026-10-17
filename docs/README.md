# Intern 核心文档目录

`docs/` 保存项目核心约束；[AGENTS.md](../AGENTS.md) 与 [CLAUDE.md](../CLAUDE.md) 提供任务阅读索引。[产品 README](../README.md) 面向使用者，帮助理解产品和上手。

| 目录 / 文档 | 内容与阅读时机 |
| --- | --- |
| [roadmap/Web优先与多应用架构-2026-10-17.md](roadmap/Web优先与多应用架构-2026-10-17.md) | 当前总路线：Web 优先、CLI 暂停扩展、app/internal 目录和 Web 推进阶段。所有开发先阅读。 |
| [roadmap/Web备份控制面与流式管道-2026-10-17.md](roadmap/Web备份控制面与流式管道-2026-10-17.md) | **首版方案已由用户确认（2026-10-18），落档于 [#11](https://github.com/flyingEirc/Intern/issues/11)**：Web 首版只做 XtraBackup 物理链——Docker 单宿主机部署（与 MySQL 容器共享 datadir 卷）、先本地发布再复制到 FTP（先内网明文 FTP 验证，随后 FTPS）、导出恢复到宿主机目录、版本准入、流式备份/增量/恢复演练、调度队列、鉴权、数据模型、Go 库清单与子任务拆分（对应总路线阶段 B–E）；未实现。实施 Web 任何子任务前阅读。 |
| [roadmap/数据库备份与恢复-2026-10-17.md](roadmap/数据库备份与恢复-2026-10-17.md) | 备份恢复详细方案：范围、选型、逻辑/物理设计、CLI 基线和历史验证；开发顺序与当前应用职责以 Web 优先总路线为准。 |
| [workflow/issue.md](workflow/issue.md) | 以 GitHub Issue 跟踪背景、功能、步骤、验收、交付；含 harness/模型和 UI/UX 证据要求。开始或调整任务时阅读。 |
| [workflow/pr.md](workflow/pr.md) | 小功能提交、提交前检查、PR 的关联 Issue 与逐项验收汇总。提交或提 PR 前阅读。 |
| [guide/development.md](guide/development.md) | 模块职责、组件复用、实现和安全约束、工作区协作、开发命令。修改代码时阅读。 |
| [guide/quality.md](guide/quality.md) | 本地与 CI 检查范围、真实组件门禁、证据和支持声明边界。测试与验收时阅读。 |
| [guide/documentation.md](guide/documentation.md) | 文档职责、路线命名、变更与冲突处理、链接和文档交付检查。修改文档时阅读。 |

## 维护分工

- `roadmap/`：描述产品功能的路线、目标、依赖、设计决策和验收边界。每个 Markdown 文档使用 `产品功能名称-YYYY-MM-DD.md`，例如 `数据库备份与恢复-2026-10-17.md`。日期不代替状态或版本依据。
- `workflow/`：描述怎么跟踪任务、提交与评审；具体执行进展写入当前仓库的 [GitHub Issues](https://github.com/flyingEirc/Intern/issues)；旧仓库 `flyingEirc/IotProject` 只保留历史，编号对照见 [Web 优先总路线第 6 节](roadmap/Web优先与多应用架构-2026-10-17.md)。
- `guide/`：描述开发中必须遵守的规范；不要另立相互冲突的产品方案。

新增核心文档时更新本目录；新增必读约束时同步两个 AI 阅读入口。此次结构调整追踪 [IotProject#11](https://github.com/flyingEirc/IotProject/issues/11)；仓库迁移后的路线与追踪统一见 [#16](https://github.com/flyingEirc/Intern/issues/16)。
