# Intern 协作入口

本文件是阅读索引。**项目核心约束维护在 `docs/`，执行任务前必须按下表阅读适用文档并遵循其中要求。** 产品介绍和使用入口见 [README.md](README.md)。

| 要了解或执行什么 | 必须阅读的位置 |
| --- | --- |
| 当前总路线、Web 优先、CLI 暂停与 app/internal 边界；Web 阶段 A–E 与 Issue、新旧仓库编号对照 | [docs/roadmap/Web优先与多应用架构-2026-10-17.md](docs/roadmap/Web优先与多应用架构-2026-10-17.md) |
| Web 首版方案（XtraBackup 物理链、Docker 部署、FTP 仓库）、子任务 P/S/R/D/V/W 与测试验收 | [docs/roadmap/Web备份控制面与流式管道-2026-10-17.md](docs/roadmap/Web备份控制面与流式管道-2026-10-17.md) |
| 备份恢复技术选型、详细设计与验收要求 | [docs/roadmap/数据库备份与恢复-2026-10-17.md](docs/roadmap/数据库备份与恢复-2026-10-17.md)；其他文档从 [docs/README.md](docs/README.md) 查找 |
| 修改代码、复用组件、处理 CLI/Web/internal 边界 | [docs/guide/development.md](docs/guide/development.md) |
| 选择检查范围、执行真实组件测试、判断能否验收 | [docs/guide/quality.md](docs/guide/quality.md)；产品矩阵细节见路线文档第 15 节 |
| 新增或修改功能、排障、拆分任务、记录交付 | [docs/workflow/issue.md](docs/workflow/issue.md) |
| 暂存、提交、准备 PR、汇总 Issue 验收 | [docs/workflow/pr.md](docs/workflow/pr.md) |
| 新增或迁移文档、命名路线、解决文档冲突 | [docs/guide/documentation.md](docs/guide/documentation.md) |
| 部署与值守 Web：镜像与 Compose 参考部署、首次管理员、TLS 与反向代理、KEK、备份与恢复 API 操作、升级回滚、故障处理、自有 FTP/FTPS 服务器自测清单 | [docs/guide/operations.md](docs/guide/operations.md) |

所有任务先读产品路线与适用 guide；涉及功能变更时先落实对应 GitHub Issue，再按 Issue/PR 流程推进。纯文档整理按文档规范和对应流程检查。未决方案与未验证内容保持明确状态；运维说明只描述仓库中真实存在的入口，不代替验收结论。

完整目录与维护职责见 [docs/README.md](docs/README.md)。Claude 的入口见 [CLAUDE.md](CLAUDE.md)。
