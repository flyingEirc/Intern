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
- P2（#27）共享入口：`physical.Preflight` 在当前执行身份下查询真实源并检查只读挂载、卷归属、权限、加密、空间；必须提供正数 `MinWorkBytes` / `MinBackupBytes`。`BackupSource` 是单次 `pipeline.Source`，调用前每次重新预检；`Metadata()` 只在进程成功、原始元数据一致且私有工作目录清理成功后返回独立副本。`CheckpointsRaw` / `InfoRaw` 是工具 `--extra-lsndir` 的原文；流内 `xtrabackup_info` 会另行渲染，`backup_size` 可能不同。`Info.UUID` 是备份 UUID，不是 `Identity.UUID` 的服务器身份。物理链发布仍由 P3 负责，不把 Source 成功当作完整备份 completed。
- Linux 物理工具通过私有继承描述符传递结构化配置，自重执行监督进程管理工具进程组及父进程死亡清理，支持父进程为容器 PID 1。凭据使用权限 `0600` 的匿名 memfd，经 `/proc/self/fd/4` 交给工具，不建立磁盘 `client.cnf`；`--no-login-paths` 与 MySQL 环境变量隔离阻止隐式凭据覆盖，工具原始诊断不进入公共错误。整个进程树被 SIGKILL 后可能遗留无凭据的工作子目录；调用方须为每次运行提供独占工作根，并由 P3 的持久归属/清理账本回收，禁止仅按前缀扫描删除。调用方 writer 必须可取消；任意卡死 Go `Write` 无法被强制中断。`physical.Extract` 仅是已认证可信 xbstream 的隔离解包原语，成功目录由调用方回收，不是完整恢复或恶意归档沙箱。
- P2 真实组件入口：在原生 Linux amd64 Docker 上执行 `go test -v -tags=integration -count=1 -timeout=16m ./app/web/integration -run '^TestP2NativeContainers$'`。缺 runtime、平台不符或缺 fixture 都失败，不 skip；测试及 CI 结果绑定当次源码、worker 程序摘要和镜像 ID。证据包含只读挂载上的全备/增量与 extra-lsndir 原文字节、可写挂载拒绝、非 root PID 1 的 memfd 全备，以及 `mysql_config_editor` 登录文件在缺少 `--no-login-paths` 时认证失败。当前候选范围与限制见控制面方案第 3 节，不能把 arm64 进程测试称为 amd64 物理链验收。
- W5（#37）恢复 API 真实组件入口：在原生 Linux amd64 Docker 上执行 `go test -v -tags=integration -count=1 -timeout=50m ./app/web/integration -run '^TestW5NativeContainers$'`（CI job `container-restore-api`，证据门禁 `.github/scripts/verify-w5-evidence.sh`，其接受/拒绝用例为 `.github/scripts/w5-gate-cases.sh`）。worker 在 `app/web/integration/restore.Dockerfile` 镜像内（`MYSQL_IMAGE`/`PXB_IMAGE` 取 `internal/physical/compat.json` 的 digest）以源库 mysql uid 运行真实的 serve：dispatcher + HTTP + `internal/restore`，对面是真实 `mysql:8.4.11`、真实 Pure-FTPd 与真实导出卷。证据只从回读状态计算：API 与 SQLite 的 restore 行、本地仓库、FTP 命名空间、导出目录权限与字节，以及用新 `mysql:8.4.11` 容器直接启动每个已发布导出后的数据快照与同一次运行记录的恢复点快照逐点一致。缺 runtime、平台不符、缺 `INTERN_W5_EXPORT_ROOT/XBSTREAM/MYSQLD` 都失败，不 skip。
- W5 终态语义补充：`restores.error_class` 是闭合集合 `unavailable|refused|restore_failed|canceled|interrupted`，其中 `refused` 专指确认摘要之后目录漂移（执行期 `plan_changed` 等），`error_code` 用引擎固定码；`side_effects` 在每一条终态行都明示，包括从未被认领、因取消而终结的排队行（三个目标都记为不存在）。已发布导出永不被取消或重启回收删除；引擎在进入导出阶段时上报 `exporting` 阶段，因此完成并演练的恢复的 `phase` 序列为 fetching、authenticating、extracting、preparing、exporting、rehearsing（逐环重复由记录端按首次出现去重）。

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

Go 格式统一由 [.golangci.yml](../../.golangci.yml) 中的 `gci`（import 分组）与 `gofumpt`（默认格式规则）维护：`mise run fmt` 自动修复，`mise run lint` 检查，覆盖 internal、CLI 与 Web。多行顶层声明（含函数/方法）之间必须留空行；连续单行函数不强制分隔。未启用 gofumpt 额外规则；`whitespace` 不负责声明间的空行检查。启用与验证记录见 [#34](https://github.com/flyingEirc/Intern/issues/34)。

`run` 与两个容器入口直接转发参数到实际程序，例如 `mise run run backup full --config /secure/source.yaml`。单 profile 默认 `mysql-9.7.1`，可用 `--profile` 或 `PROFILE=... mise run test:container` 指定；CLI flag 优先。帮助命令不会运行真实恢复场景，门禁是否通过仍须提供实际测试证据。

Web 默认只监听 `127.0.0.1:8080`。登录 Cookie 固定带 `Secure`；非回环监听必须配置内置 TLS（`--tls-cert-file` / `--tls-key-file`，最低 TLS 1.2），或同时设置 `--tls-terminated-by-proxy` 与 `--trusted-proxies`，显式声明前置 HTTPS 代理。仅配置受信代理 IP 不代表已启用 HTTPS。代理模式的后端链路仍是明文，必须由网络/防火墙隔离直连访问；默认不信任任何 `X-Forwarded-For`。回环 HTTP 仅供本地开发使用。

同机 HTTPS 反向代理也应显式配置，例如 `--listen 127.0.0.1:8080 --tls-terminated-by-proxy --trusted-proxies 127.0.0.1,::1`；未信任代理时所有转发请求都使用代理地址，登录限流会被所有用户共享。只有实际的代理地址才应加入信任范围。

仓库、数据源与策略 API 的凭据与预检设置全部可选：`--kek-file`（密封数据源与 FTP 密码的独立 KEK）、`--work-root`、`--xtrabackup-path`，以及必须同时为正的 `--min-work-bytes` 与 `--min-backup-bytes`。未配置时服务启动与既有接口行为不变，只有依赖这些输入的端点返回 `503 unavailable` 与固定文案；KEK 必须位于 `metadata_dir`、`work_root` 之外，创建或修改本地仓库根目录时也会校验该根目录不包含 KEK。数据源与 FTP 密码用 Tink AES-128-GCM 加密存储，AAD 绑定 `表:行ID:字段`，因此错误 KEK、跨行或跨字段移动的密文都会失败；API 只写不读，响应仅含 `has_secret`，FTP 仓库另有 `plaintext_credentials: true`。显式明文 FTP 的创建/更新在同一事务写入 `detail.plaintext_ftp=true` 的成功审计；`tls_mode` 必须显式为 `plain`，省略或 `explicit`/`implicit`（FTPS）一律 `422`，当前仅回环开发为明文 HTTP。预检失败返回 `409 preflight_failed` 且不写库：创建或启用失败时策略保持未启用；已经启用的策略在更新预检失败时保持启用及原有设置。未配置 KEK 时写入数据源或带密码的 FTP 仓库返回 `503 unavailable`，本地仓库探测不需要 KEK。`kind` 与 `tls_mode` 在 PUT 中不可变，形状不符返回 `422 validation_failed`。时区必须是非空 IANA 名称，`Local` 与空字符串按 `validation_failed` 拒绝。数据源预检无论成败都持久化 `last_preflight_json`，仅在身份检查成功时写入服务器身份。

恢复输入同样可选：`--export-root`、`--xbstream-path`、`--mysqld-path`、`--restore-expansion-factor`（默认 8）与 `--restore-use-memory`（字节，默认 0 表示省略该参数）。`/api/v1/restores/plan` 只读：不写 `restores` 行、不写文件、不写 FTP，返回链、逐环来源（local/ftp）、LSN、空间估算、导出位置、风险码、`plan_digest` 与 `rehearsal_available`；拒绝以 `restore_refused` 返回，机器码放在 `error.detail.code`（404 `backup_not_found`、422 `matrix_unsupported`、其余 409，运行时未配置为 503）。`POST /api/v1/restores` 仅 admin，要求二次密码（错误 403 `confirmation_failed`、按用户 5 次/分钟限流、超限 429 带 `Retry-After`），`plan_digest` 必须等于请求时重新构建的计划，否则 409 `plan_changed`；`rehearse=true` 而未配置 `mysqld_path` 为 422 `validation_failed`。恢复行与成功审计同事务提交，审计失败则回滚。缺 KEK、work root、xtrabackup、xbstream 或 export root 时计划与执行返回 503；导出根必须是绝对路径、非符号链接、位于 metadata 目录、work root 与任何本地仓库根之外，且不得包含 KEK。恢复与备份 run 共享一个全局执行槽：任一方 running 时另一方不被认领。取消 queued 恢复直接 canceled，取消 running 恢复置 `cancel_requested` 并取消其上下文；已发布导出（原子 rename 到 `<export_root>/<restore_id>`）不会被取消或重启恢复删除，`side_effects` 明示工作目录/待发布导出是否被回收以及导出是否已发布。启动恢复先处理遗留 running run，再将遗留 running 恢复记为 failed/interrupted 并按精确 id 回收两个目标，最后执行物理与复制恢复。

W5 恢复 API 的裁定边界：`verify_sql` 由恢复引擎唯一裁定，API 不做第二套限制——`internal/restore` 导出 `ValidateVerifySQL`、`MaxVerifySQLStatements`（16）与 `MaxVerifySQLBytes`（4096），`POST /api/v1/restores` 调用同一函数校验，OpenAPI 的 `maxItems`/`maxLength`/`minLength` 与这两个常量一致并有测试比对，因此被接受的请求不会在认领时以 `invalid_verify_sql` 失败。二次密码限流按用户共享一个桶：`PUT /auth/password` 与恢复确认消耗同一预算，每用户每分钟合计 5 次（不是每条路由各 5 次）。`restore_id` 是 UUIDv7 字节的小写十六进制（`^[0-9a-f]{32}$`，无连字符），id 顺序即创建顺序，故 `GET /api/v1/restores` 的 `id DESC` 分页确实最新在前，且 id 仍满足 `restore.Recover` 要求的 32 hex 形式。导出根的位置校验分页读取全部本地仓库根，不只第一页，仓库数量超过一页时不会有根被跳过。引擎返回非类型化或 context 错误（自身没有清理观察）时，`side_effects` 由适配层按与成功路径相同的方式实测两个精确目标与已发布导出得出，绝不以全 false 宣称“未删除任何东西”；引擎上报了自身观察的 `ExecError` 保持原样、不被覆盖。取消请求若落在“已认领但上下文尚未注册”的窗口，worker 在注册上下文后重读 `cancel_requested` 并立即取消，请求不会丢失。

用户创建、修改、改密、角色/禁用、会话吊销及登出的数据库变更与成功审计同事务提交；审计写入失败则变更不生效。失败/拒绝请求和登录审计仍尽力记录，Cookie 传输与改密后的 token 轮换不属于数据库事务，不承诺网络与数据库原子性。每进程密码哈希总并发为 2、匿名登录最多占 1；过载仍以 `503` 与 `Retry-After` 拒绝，不采用无界等待。会话响应缓冲最多 8 MiB，超限或尝试流式输出返回单一 `500`，不静默截断 `200`；这不是整进程内存上限，也不是 SSE 支持。

`--shutdown-timeout` 是优雅关闭窗口，不是整个进程的绝对退出上限。窗口结束后取消请求并关闭连接，再等待脱离审计的截止窗口、SQLite `busy_timeout` 和少量调度余量（当前额外最多 10.5 秒）。SQLite busy 回调及内核磁盘 I/O 不保证能被 context 立即中断；超过排空窗口仍会报告错误，不承诺有限时间排空任意卡死 I/O。部署的停止宽限期应覆盖这些窗口。

控制面库只有一条写连接（`SetMaxOpenConns(1)`）。因此写事务不能继承调用方的可取消 context：调用方在事务中途被取消时，连接可能停留在半开事务上，此后每次写入（包括系统审计）都要等满 `busy_timeout` 才报 `SQLITE_BUSY`，直到进程重启。`withTx`/`withTxDB` 因此只在开始前查询调用方 context —— 已结束则直接返回其错误，否则 `BeginTx`、每条语句以及 `Commit`/`Rollback` 都在 `context.WithoutCancel` 加 30 秒上限的窗口内执行。调用方被取消不会中断已开始的写事务，也绝不会留下半开事务；取消的语义是"这次写仍然完成"，不是"放弃这次写"。需要取消语义的调用点必须自己在进入事务前判断，或者像 dispatcher 的终态写一样显式使用脱离窗口。

panic 发生在响应输出前时，可以丢弃部分缓冲并返回统一 `500`；字节已发送后无法收回或改写状态，只记录错误，不能把这类情形描述为已回滚。改密事务成功后若 token 轮换失败，密码与成功审计仍保留，旧会话不得被提升到新 epoch，需要用新密码重新登录。

`check` 严格顺序执行 build → test-race → lint，任一步失败即停止，不用并行依赖替代。提交 hook 通过 mise 运行分类器，并在独立暂存快照中调用同一个 `check` 任务；纯文档/非业务变更仍按原规则豁免。CI 使用相同锁定版本和检查规则：活跃 CI 为 CircleCI（`.circleci/config.yml`），对每次推送（功能分支、PR 分支与合并后的 main）运行同一矩阵与全部 `verify-*-evidence.sh` 门禁，不做纯文档跳过；`.github/workflows/quality.yml` 仅保留手动触发（workflow_dispatch），用于计费恢复后的对照，须与 `.circleci/config.yml` 保持同步。实现与实际运行状态由 [当前仓库 #4](https://github.com/flyingEirc/Intern/issues/4) 跟踪。workflow 已提交不代表真实容器矩阵或合并门禁已通过验收。

项目文件直接位于 Git 根目录：`.github/`、`.githooks/`、`.circleci/`、`app/`、`docs/`、`internal/`、`go.work` 与 `mise.toml` 同级，没有额外的 `Intern/` 包装目录。GitHub workflow 与 CircleCI 配置都从仓库根执行，缓存、测试证据和制品路径也以根为基准。本地 hook 配置为 `git config core.hooksPath .githooks`；新 clone 后执行此命令启用暂存快照检查。
