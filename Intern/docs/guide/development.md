# 开发规范

开发前阅读 [Web 优先总路线](../roadmap/Web优先与多应用架构-2026-10-17.md) 和 [备份恢复详细方案](../roadmap/数据库备份与恢复-2026-10-17.md)，按其中的范围、职责、选型、安全与验收要求实施。任务跟踪见 [Issue 流程](../workflow/issue.md)，提交见 [PR 流程](../workflow/pr.md)，质量门禁见 [quality.md](quality.md)。

当前主线为完成 Web；CLI 暂停新功能，只做必要维护和共享接口兼容。Web 前端栈及首版范围由 [#18](https://github.com/flyingEirc/IotProject/issues/18) 决策，不能在本轮目录调整中静默指定。

## 目录与共享接口

| 项目根目录下的路径 | 职责 |
| --- | --- |
| `internal/` | CLI/Web 共享的备份恢复、制品、密钥、元数据和调度能力；不能依赖产品入口。 |
| `app/cli/` | CLI 命令、配置与终端交互；保留兼容，暂停功能扩展。 |
| `app/web/` | 当前主线 Web 产品和后端；前端栈尚未确定，当前目录占位。 |
| `app/site/` | 个人产品站点，不承担备份控制面。 |
| `mobile/` | 后续可能的客户端，当前不作实现承诺。 |
| `docs/` | 核心路线、流程与开发约束，按 [文档规范](documentation.md) 维护。 |

- 共用业务优先进入 `internal/`，产品入口负责协议、参数和交互，避免 CLI/Web 各复制恢复引擎。
- 当前 `go.work` 只列 `internal/` 与 `app/cli/`，模块路径分别为 `github.com/flyingEirc/IotProject/internal` 和 `github.com/flyingEirc/IotProject/app/cli`；CLI replace 指向 `../../internal`。Web/site 未有实现前不创建空模块。
- 使用 `go.work` 管理真实存在的 Go 模块。新增模块同时维护 `go.mod` 和工作区，不为空目录虚构模块或预造空抽象。
- 涉及 `internal/` 的改动必须在 Issue/PR 说明对 CLI/Web 的接口和行为影响及合入依赖。

## 组件与实现

- 缺少组件先搜索、评估成熟开源方案；引入前核实维护状态、许可证、目标版本兼容性与实际运行条件，记录来源、版本、取舍和验证。
- 不静默替换已定技术栈。Cobra、Gin、sqlc、goose、slog、Viper、jsonv2 等边界以路线为准；sqlc 不是运行时 ORM，Cobra 不替代 HTTP 框架。
- 推荐、暂定、待确认、未实测不代表采纳、实现或验证；不得借调研扩大支持范围。选型、方案或验收标准变化先在 Issue 明确决策，再同步路线。
- 参考实现不能成为本项目的隐含约束。复用代码遵守许可、版权与声明；Databasus 等参考路径以项目根和实际布局为准，不能把参考方案直接当作 Intern 已支持能力。
- SQL 变更维护源查询和生成代码，必要时运行 `mise run generate`；模式变更通过版本化迁移，不手改生成结果规避源定义。
- 官方数据库工具使用明确的可执行文件与参数调用，维护进程生命周期、取消和错误传播；不自研 redo/binlog 协议或拼接 shell 来执行用户参数。

## 安全与恢复约束

- 加密固定 AES-128-GCM，压缩固定 Zstandard，先压缩后加密；复用成熟认证流组件，保证 nonce 唯一和完整流认证，不自制格式替换已定方案。
- 密钥与备份分离；密码、明文密钥、token、完整 DSN 不进入日志、清单或提交。配置仅含示例值或 secret 引用，真实测试凭据和产物不入库。
- 备份目录与 state 分离，目录权限 `0700`；KEK 独立保存、权限 `0600`。含明文密码的 YAML 同样要求 `0600`，不承诺全链路无明文工作文件。
- 恢复以显式计划为入口，默认隔离空目标；完整认证和范围检查通过后才能写目标。不得把失败恢复描述成已回滚，部分 SQL 副作用需隔离处理。
- 失败、取消、短写、工具退出或 Close 错误不能发布成功或推进连续保护水位。涉及安全与恢复行为必须按路线和 [质量门禁](quality.md) 验收。

## 工作区与分支

- 修改前检查 Git 状态，区分本任务与其他未提交工作；不得覆盖、回退或提交他人变更。
- CLI 与 Web 分别在各自独立分支开发；名称沿用仓库现有约定，不因本规范自行强制命名。
- 未被要求时，不自动创建、切换或合并分支，也不主动推送。共享变更先协调接口，再按明确依赖合入。

## 常用开发入口

工具版本与任务统一维护在 [mise.toml](../../mise.toml)：Go `1.27.0`（与 `go.work` 一致）、golangci-lint `2.13.2`，sqlc 生成命令固定 `v1.30.0`。首次使用执行 `mise trust`、`mise install`，不要另外维护 Taskfile 或依赖开发机随机 PATH 中的工具版本。工具/任务调整依据见 [#14](https://github.com/flyingEirc/IotProject/issues/14)。

以下命令在 Intern 项目目录执行；在其子目录运行时，mise 仍以配置所在的 Intern 目录执行任务。也可从其他目录使用 `mise -C /path/to/Intern run <任务>`。

| 目的 | 命令 |
| --- | --- |
| 构建 CLI 到 `bin/intern` | `mise run build` |
| 查看开发入口帮助 | `mise run run --help` |
| 快速回归 | `mise run test` |
| 业务提交完整本地检查 | `mise run check`（build、test-race、golangci-lint） |
| 单独 lint / 格式整理 | `mise run lint` / `mise run fmt` |
| 重新生成 sqlc | `mise run generate` |
| 生产与集成代码 lint | `mise run lint:integration` |
| 所有锁定 profile 的真实容器门禁 | `mise run test:containers` |
| 单个 profile 的真实容器门禁 | `mise run test:container --profile mysql-9.7.1` |

`run` 与两个容器入口直接转发参数到实际程序，例如 `mise run run backup full --config /secure/source.yaml`。单 profile 默认 `mysql-9.7.1`，可用 `--profile` 或 `PROFILE=... mise run test:container` 指定；CLI flag 优先。帮助命令不会运行真实恢复场景，门禁是否通过仍须提供实际测试证据。

`check` 严格顺序执行 build → test-race → lint，任一步失败即停止，不用并行依赖替代。提交 hook 通过 mise 运行分类器，并在独立暂存快照中调用同一个 `check` 任务；纯文档/非业务变更仍按原规则豁免。CI 使用相同锁定版本和检查规则，工作流位于实际 Git 根的 `.github/workflows/quality.yml`；实现与实际运行状态由 [当前仓库 #4](https://github.com/flyingEirc/Intern/issues/4) 跟踪。workflow 已提交不代表真实容器矩阵或分支保护已通过验收。

实际 Git 根当前位于 Intern 的上一级；GitHub workflow 须位于真实 Git 根的 `.github/workflows/`，工作目录设置为 Intern。不要在 `Intern/.github/` 建立无法被识别的门禁副本。本地 hook 当前配置 `core.hooksPath=Intern/.githooks`；新 clone 或布局变化后应按实际 Git 根与项目路径设置。
