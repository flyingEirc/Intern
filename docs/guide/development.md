# 开发规范

开发前阅读 [Web 优先总路线](../roadmap/Web优先与多应用架构-2026-10-17.md) 和 [备份恢复详细方案](../roadmap/数据库备份与恢复-2026-10-17.md)；Web 子任务另读 [Web 备份控制面与流式管道](../roadmap/Web备份控制面与流式管道-2026-10-17.md)，按其中的范围、职责、选型、安全与验收要求实施。任务跟踪见 [Issue 流程](../workflow/issue.md)，提交见 [PR 流程](../workflow/pr.md)，质量门禁见 [quality.md](quality.md)。

当前主线为完成 Web；CLI 暂停新功能，只做必要维护和共享接口兼容。Web 首版范围与前端栈已确认（[#11](https://github.com/flyingEirc/Intern/issues/11)，见 [Web 优先总路线第 3 节](../roadmap/Web优先与多应用架构-2026-10-17.md)），不得静默更换。前端暂缓，当前按阶段 B–E 的子 Issue 先实施后端；对前端暴露的接口以 OpenAPI 契约 `app/web/api/openapi.yaml` 为准，接口变更须同步契约与测试（[#18](https://github.com/flyingEirc/Intern/issues/18)）。契约测试使用 kin-openapi 校验规范本身、Gin 路由与规范操作双向一致，以及 API 测试中的全部请求与响应；后端不做代码生成（[#22](https://github.com/flyingEirc/Intern/issues/22)）。

## 目录与共享接口

| 项目根目录下的路径 | 职责 |
| --- | --- |
| `internal/` | CLI/Web 共享的备份恢复、制品、密钥、元数据和调度能力；不能依赖产品入口。 |
| `app/cli/` | CLI 命令、配置与终端交互；保留兼容，暂停功能扩展。 |
| `app/web/` | 当前主线 Web 产品和后端：Cobra 入口（`serve`、`migrate`、`user create-admin`）、Gin + `encoding/json/v2` 协议层、SQLite / goose / sqlc 控制面库、scs 服务端会话与 admin / operator / viewer 三角色、变更审计，以及 `api/openapi.yaml` 契约；前端（React + TypeScript + Vite + Ant Design + TanStack Query）暂缓到阶段 F。 |
| `app/site/` | 个人产品站点，不承担备份控制面。 |
| `mobile/` | 后续可能的客户端，当前不作实现承诺。 |
| `docs/` | 核心路线、流程与开发约束，按 [文档规范](documentation.md) 维护。 |

- 共用业务优先进入 `internal/`，产品入口负责协议、参数和交互，避免 CLI/Web 各复制恢复引擎。
- `go.work` 列出 `internal/`、`app/cli/`、`app/web/` 三个模块，模块路径分别为 `github.com/flyingEirc/Intern/internal`、`github.com/flyingEirc/Intern/app/cli`、`github.com/flyingEirc/Intern/app/web`；CLI 与 Web 的 replace 均指向 `../../internal`。site 未有实现前不创建空模块。
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
- 提交前的失败、取消、短写、工具退出或 Close 错误不能发布成功或推进连续保护水位。对象 `Commit` 返回成功，或本地发布已完成 rename 与持久化后，随后到达的取消不撤销该对象；单个对象成功不自动等于完整备份 `completed`（PR #26 用户确认）。涉及安全与恢复行为必须按路线和 [质量门禁](quality.md) 验收。

## 工作区与分支

- 修改前检查 Git 状态，区分本任务与其他未提交工作；不得覆盖、回退或提交他人变更。
- CLI 与 Web 分别在各自独立分支开发；名称沿用仓库现有约定，不因本规范自行强制命名。
- 未被要求时，不自动创建、切换或合并分支，也不主动推送。共享变更先协调接口，再按明确依赖合入。

## 常用开发入口

工具版本与任务统一维护在 [mise.toml](../../mise.toml)：Go `1.27.0`（与 `go.work` 一致）、golangci-lint `2.13.2`，sqlc 生成命令固定 `v1.30.0`。首次使用执行 `mise trust`、`mise install`，不要另外维护 Taskfile 或依赖开发机随机 PATH 中的工具版本。工具/任务调整依据见 [IotProject#14](https://github.com/flyingEirc/IotProject/issues/14)。

以下命令在仓库根目录执行；在其子目录运行时，mise 仍以根目录的 mise.toml 所在目录执行任务。也可从其他目录使用 `mise -C /path/to/Intern run <任务>`。

| 目的 | 命令 |
| --- | --- |
| 构建 CLI 与 Web 后端到 `bin/intern`、`bin/intern-web` | `mise run build` |
| 启动 Web 后端（监听前自动迁移元数据库） | `./bin/intern-web serve --metadata-dir <绝对路径> --listen 127.0.0.1:8080` |
| 查看或执行 Web 元数据库迁移 | `./bin/intern-web migrate status` / `./bin/intern-web migrate up` |
| 创建初始管理员（serve 运行期间也可执行） | `./bin/intern-web user create-admin --username <用户名> --metadata-dir <绝对路径>`（终端无回显输入两次；脚本使用 `--password-stdin` 从标准输入读一行，不接受命令行明文） |
| 查看开发入口帮助 | `mise run run --help` |
| 快速回归 | `mise run test` |
| 业务提交完整本地检查 | `mise run check`（build、test-race、golangci-lint） |
| 单独 lint / 格式整理 | `mise run lint` / `mise run fmt` |
| 重新生成 sqlc | `mise run generate` |
| 生产与集成代码 lint | `mise run lint:integration` |
| 所有锁定 profile 的真实容器门禁 | `mise run test:containers` |
| 单个 profile 的真实容器门禁 | `mise run test:container --profile mysql-9.7.1` |

`run` 与两个容器入口直接转发参数到实际程序，例如 `mise run run backup full --config /secure/source.yaml`。单 profile 默认 `mysql-9.7.1`，可用 `--profile` 或 `PROFILE=... mise run test:container` 指定；CLI flag 优先。帮助命令不会运行真实恢复场景，门禁是否通过仍须提供实际测试证据。

Web 默认只监听 `127.0.0.1:8080`。登录 Cookie 固定带 `Secure`；非回环监听必须配置内置 TLS（`--tls-cert-file` / `--tls-key-file`，最低 TLS 1.2），或同时设置 `--tls-terminated-by-proxy` 与 `--trusted-proxies`，显式声明前置 HTTPS 代理。仅配置受信代理 IP 不代表已启用 HTTPS。代理模式的后端链路仍是明文，必须由网络/防火墙隔离直连访问；默认不信任任何 `X-Forwarded-For`。回环 HTTP 仅供本地开发使用。

同机 HTTPS 反向代理也应显式配置，例如 `--listen 127.0.0.1:8080 --tls-terminated-by-proxy --trusted-proxies 127.0.0.1,::1`；未信任代理时所有转发请求都使用代理地址，登录限流会被所有用户共享。只有实际的代理地址才应加入信任范围。

用户创建、修改、改密、角色/禁用、会话吊销及登出的数据库变更与成功审计同事务提交；审计写入失败则变更不生效。失败/拒绝请求和登录审计仍尽力记录，Cookie 传输与改密后的 token 轮换不属于数据库事务，不承诺网络与数据库原子性。每进程密码哈希总并发为 2、匿名登录最多占 1；过载仍以 `503` 与 `Retry-After` 拒绝，不采用无界等待。会话响应缓冲最多 8 MiB，超限或尝试流式输出返回单一 `500`，不静默截断 `200`；这不是整进程内存上限，也不是 SSE 支持。

`--shutdown-timeout` 是优雅关闭窗口，不是整个进程的绝对退出上限。窗口结束后取消请求并关闭连接，再等待脱离审计的截止窗口、SQLite `busy_timeout` 和少量调度余量（当前额外最多 10.5 秒）。SQLite busy 回调及内核磁盘 I/O 不保证能被 context 立即中断；超过排空窗口仍会报告错误，不承诺有限时间排空任意卡死 I/O。部署的停止宽限期应覆盖这些窗口。

panic 发生在响应输出前时，可以丢弃部分缓冲并返回统一 `500`；字节已发送后无法收回或改写状态，只记录错误，不能把这类情形描述为已回滚。改密事务成功后若 token 轮换失败，密码与成功审计仍保留，旧会话不得被提升到新 epoch，需要用新密码重新登录。

`check` 严格顺序执行 build → test-race → lint，任一步失败即停止，不用并行依赖替代。提交 hook 通过 mise 运行分类器，并在独立暂存快照中调用同一个 `check` 任务；纯文档/非业务变更仍按原规则豁免。CI 使用相同锁定版本和检查规则，工作流位于实际 Git 根的 `.github/workflows/quality.yml`；实现与实际运行状态由 [当前仓库 #4](https://github.com/flyingEirc/Intern/issues/4) 跟踪。workflow 已提交不代表真实容器矩阵或分支保护已通过验收。

项目文件直接位于 Git 根目录：`.github/`、`.githooks/`、`app/`、`docs/`、`internal/`、`go.work` 与 `mise.toml` 同级，没有额外的 `Intern/` 包装目录。GitHub workflow 从仓库根执行，缓存、测试证据和制品路径也以根为基准。本地 hook 配置为 `git config core.hooksPath .githooks`；新 clone 后执行此命令启用暂存快照检查。
