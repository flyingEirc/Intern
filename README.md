# Intern

**为 MySQL 提供可验证的备份与隔离恢复。**

Intern 是一个正在开发的数据库备份与恢复工具，帮助你建立全量备份、归档后续变化，并在需要时恢复到已保存的检查点。当前研发主线优先完成 Web：Web 后端（控制面 API、XtraBackup 物理链、FTP 副本与隔离恢复）的阶段 B、C、D 已通过真实容器 CI 原生验收并合入 `main`；CLI 暂停新功能、保留必要维护。**Web 首版仍未交付**——没有前端页面；阶段 E 的 Docker 部署与 FTPS（D1、S2）及候选制品门禁（G）已原生验收，V1 内网实测于 2026-11-05 取消（FTP/FTPS 由每次涉及代码或配置的合并前手动运行、main 推送与每日定时运行的真实容器 CI 覆盖），阶段 E 已关闭。

## 当前可复用的能力基线

**CLI 基线：单机、本地文件系统、单业务库**

| 能力 | 当前行为 |
| --- | --- |
| 逻辑全量备份 | 通过官方 `mysqldump` 导出 InnoDB 一致性快照，包含视图、存储程序、触发器和事件。全量期间须避免并发 DDL。 |
| binlog 增量 | 轮转并归档关闭的 binlog 文件，在全量基线后形成可追溯的增量链。 |
| 检查点恢复 | 选择全量或增量备份 ID，校验完整链后生成恢复计划；显式执行时恢复到隔离目标。 |
| 定时备份 | 每日指定时间全量、按间隔归档增量，支持 IANA 时区；前台常驻运行。 |
| 压缩与加密 | 可配置 Zstandard 压缩和 Tink AES-128-GCM 认证加密，密钥独立保存。 |
| 配置与自动化 | YAML、环境变量、命令行 flags；结果以 JSON 输出，状态与目录索引存入本地 SQLite。 |

已有验证记录覆盖 macOS arm64、MySQL 与官方客户端 9.7.1 下的真实原生进程备份恢复。该记录不代表所有版本、平台或生产环境已验收；详见 [路线第 14 节](docs/roadmap/数据库备份与恢复-2026-10-17.md)。真实容器矩阵已在 Linux amd64 的 GitHub Actions 自托管 runner 上，对 MySQL 9.7.1 与 8.4.11 两个锁定配置各 14 个场景通过：`main` `2a2040d` 的 [run 41871999890](https://github.com/flyingEirc/Intern/actions/runs/41871999890)，门禁加固分支 `c5ebc93` 第 2 次尝试的 [run 41907540485](https://github.com/flyingEirc/Intern/actions/runs/41907540485)。这是该矩阵与平台的证据，仍不外推到其他版本、平台或生产环境；剩余项与逐 Issue 结论见 [收尾与验收 #1](https://github.com/flyingEirc/Intern/issues/1) 及 #2–#4。

**Web 后端能力（阶段 B–D 已原生验收并合入 `main`；无前端）**

| 能力 | 当前行为与证据边界 |
| --- | --- |
| 控制面 API、会话与三角色 | `/api/v1` 契约（`app/web/api/openapi.yaml`）由测试校验实现一致；`intern_session` cookie 固定 `Secure`/`HttpOnly`/`SameSite=Lax`，`admin`/`operator`/`viewer` 权限矩阵与审计、登录与改密限速、独占迁移与状态命令。 |
| XtraBackup 物理全量与页增量 | 真实 MySQL 8.4.11 + PXB 8.4.0-7、linux/amd64 容器 CI 完成全量 → 增量 1 → 增量 2、链管理、本地仓库发布与清理账本、进程故障与准入拒绝、SIGKILL 重启回收。 |
| FTP 副本复制 | 发布后把密文原样复制到运维 FTP 服务器（真实 Pure-FTPd 1.0.54，明文模式）并完整回读认证，认证通过才 `verified`；FIFO 顺序、重试、`.part` 回收；未 `verified` 不会被保留判定删除。 |
| 隔离恢复、导出与演练 | 只读恢复计划 + `plan_digest` 与管理员密码二次确认执行、按链 prepare、导出到宿主机目录并由新 MySQL 8.4.11 容器启动核对独立快照（含**仅从 FTP 副本**分别恢复增量 1 与增量 2）；拒绝全部发生在 prepare 与写导出之前；取消、SIGKILL 与 ENOSPC 精确回收；备份与恢复全局互斥。 |

Web 阶段的候选提交、流水线与限制见 [总路线第 5 节](docs/roadmap/Web优先与多应用架构-2026-10-17.md)。阶段 D 的证据覆盖上述组件组合本身，**版本矩阵仍是 `candidate`（`verified: false`）**，不外推到所有版本、平台或生产环境；阶段 E 的镜像与 Compose 部署（D1 [#41](https://github.com/flyingEirc/Intern/issues/41)）、FTPS（S2 [#42](https://github.com/flyingEirc/Intern/issues/42)）与候选制品/契约门禁（G [#44](https://github.com/flyingEirc/Intern/issues/44)）已在候选 `9489f6c` 的 [run 41859225449](https://github.com/flyingEirc/Intern/actions/runs/41859225449) 原生验收（14 个 job 全绿）。V1 内网实测（[#43](https://github.com/flyingEirc/Intern/issues/43)）于 2026-11-05 取消，FTP/FTPS 改由每次涉及代码或配置的合并前手动运行、main 推送与每日定时运行的真实容器 CI 以固定 Pure-FTPd 1.0.54 夹具覆盖；截至 2026-11-05 最近一次整套 main 运行为 merge `7355766` 的 [run 41975617492](https://github.com/flyingEirc/Intern/actions/runs/41975617492)（14/14 job 全绿），非夹具服务器无证据。

## 目录与当前路线

| 目录 | 作用与当前状态 |
| --- | --- |
| `app/web/` | 当前产品主线。阶段 B（[#12](https://github.com/flyingEirc/Intern/issues/12)、#22–#24）、阶段 C（P2 #27、S1 #28、P3 #29、R1 #30、W3 #31、W4 #32，后端验收 C-A1–C-A5）与阶段 D（P4 #36、W5 #37，验收 P4-A1–A9、W5-A1–A8）均已通过固定候选的 Linux amd64 原生验收并合入 `main`；候选提交、CI 流水线与证据边界见 [总路线第 5 节](docs/roadmap/Web优先与多应用架构-2026-10-17.md)。阶段 E 的 D1、S2 与 G 已在候选 `9489f6c` 验收并随 [PR #45](https://github.com/flyingEirc/Intern/pull/45) 合入 `main`（merge `2a2040d`），V1 内网实测取消（2026-11-05），阶段 E 已关闭并完成后端首版交付；接口以 [OpenAPI 契约](app/web/api/openapi.yaml) 为唯一契约，前端 F（暂缓）、Web 首版与物理 PITR 均未交付。CI 与合并门禁为 GitHub Actions（[`.github/workflows/quality.yml`](.github/workflows/quality.yml)），运行在 WSL2 里的 Linux 自托管 runner 上：分支推送只跑快速检查，合并前在 PR 分支手动触发、按改动的 CLI/Web 层运行容器验收，main 的业务推送、tag 与每日定时运行全部检查；只改文档或 CI 自身文件（`.github/`）的推送不创建运行，分层由 `.github/scripts/needs-ci.sh` 判定（见 [质量规范](docs/guide/quality.md)）。 |
| `app/cli/` | 已有 CLI 实现与兼容入口，暂停新功能。 |
| `app/site/` | 产品站点目录，后续独立安排。 |
| `internal/` | 各应用共享的备份恢复、制品、密钥、元数据和调度能力。 |

总路线与阶段验收见 [Web 优先与多应用架构](docs/roadmap/Web优先与多应用架构-2026-10-17.md)。`mise run build` 同时构建 CLI 与 Web 后端；Web 的部署、首次管理员、TLS、KEK、备份与恢复操作、升级与排障见 [运维使用说明](docs/guide/operations.md)，开发命令见 [开发入口](docs/guide/development.md#常用开发入口)，接口定义见 [OpenAPI 契约](app/web/api/openapi.yaml)。以下备份恢复示例仍用于现有 CLI 基线；Web 后端的操作全部通过 `/api/v1`。

## 从源码开始

准备 mise，以及与源/目标 MySQL 匹配的官方 `mysql`、`mysqldump`、`mysqlbinlog`。工具准入不等于该版本已完成恢复验证。源库需要 binlog、ROW 格式和对应备份/日志读取/轮转权限；当前已记录的恢复验证使用 GTID ON。

Go `1.27.0`、golangci-lint `2.13.2` 和开发任务统一声明在 [mise.toml](mise.toml) 中，Go 版本与 `go.work` 保持一致。首次使用时阅读配置后信任并安装工具。

在克隆后的仓库根目录（与 `.github/`、`go.work` 和 `mise.toml` 同级）执行：

```sh
mise trust
mise install
mise run build
./bin/intern --help
./bin/intern-web --help
```

准备三个独立位置：备份目录、状态目录和密钥目录。以下使用当前用户的 home 目录演示：

```sh
mkdir -p "$HOME/intern/backups" "$HOME/intern/state" "$HOME/intern-secrets"
chmod 700 "$HOME/intern/backups" "$HOME/intern/state" "$HOME/intern-secrets"
cp app/cli/example.yaml "$HOME/intern-secrets/source.yaml"
chmod 600 "$HOME/intern-secrets/source.yaml"
```

编辑 `source.yaml`，将连接信息改为实际源库，将 `backup.directory`、`backup.state_directory`、`backup.key_file` 改为上面目录的**绝对路径**。YAML 不会自动展开示例中的 `$HOME`。TCP 默认校验服务器身份，需要正确 CA 与匹配的主机名；本地 Unix socket 的配置见 [示例](app/cli/example.yaml)。

示例通过 `password_env: INTERN_MYSQL_PASSWORD` 引用密码。运行前用你的 secret 管理方式将该环境变量注入当前进程，避免把真实密码写入命令历史、日志或仓库。

```sh
# 创建独立密钥；此文件必须另外备份，已有文件不会被覆盖。
./bin/intern keygen --config "$HOME/intern-secrets/source.yaml"

# 建立全量基线，随后归档增量并查看已发布备份。
./bin/intern backup full --config "$HOME/intern-secrets/source.yaml"
./bin/intern backup incremental --config "$HOME/intern-secrets/source.yaml"
./bin/intern backup list --config "$HOME/intern-secrets/source.yaml"

# 可选：前台常驻，每天上海时间 02:00 全量，每 15 分钟增量。
./bin/intern schedule run --config "$HOME/intern-secrets/source.yaml" \
  --full-at 02:00 --incremental-every 15m --timezone Asia/Shanghai
```

配置优先级：**显式 flags > `INTERN_` 环境变量 > YAML > 默认值**。一次性命令退出后不再归档；调度进程退出后调度也停止。

## 恢复到隔离目标

复制源配置为 `target.yaml`，将 `connection` 改为不同实例上的空目标库及目标账号，保留原备份目录、state 目录和 KEK 引用。源库不需要在线。目标密码应使用独立 secret 引用，避免沿用源端凭据。

```sh
# 用 backup list 返回的实际 ID 替换 BACKUP_ID；默认校验并输出计划。
./bin/intern restore --config "$HOME/intern-secrets/target.yaml" --backup BACKUP_ID

# 阅读并确认计划、目标和备份链后，显式写入隔离目标。
./bin/intern restore --config "$HOME/intern-secrets/target.yaml" --backup BACKUP_ID --execute
```

目标不能是源实例，目标库必须为空。简单 ROW 表可选择新库名；含视图、存储程序、触发器或事件时，在隔离实例保持原库名。恢复执行中断可能留下部分 SQL 副作用，失败目标需隔离处理。

灾备必须保留 **备份目录 + 停止调度后完整的 state 目录 + 独立 KEK**。当前目录索引含绝对路径，异机恢复应保持相同路径；没有离线扫描重建索引或自动重定位能力。不要手工删除仍被增量引用的全量或中间备份。

## 当前边界与后续路线

- CLI 当前增量是 binlog 归档（逻辑链）；Web 后端已实现并通过真实容器 CI 的是 XtraBackup 物理全量与页增量链。两者的环境、版本与验证互不覆盖。
- 当前按已封存检查点恢复；任意时间输入、实例级 PITR、跨库事务恢复尚不在已交付能力中（N1）。
- Web 前端（F [#19](https://github.com/flyingEirc/Intern/issues/19)）、S3、自动保留清理（N2）与持续断线归档尚未交付；Docker 单宿主机部署（D1）与 FTPS（S2）已在候选 `9489f6c` 原生验收；V1 内网实测于 2026-11-05 取消，FTP/FTPS 只由固定 Pure-FTPd 1.0.54 夹具的容器 CI 证明，其他服务器须运维按[运维说明第 12 节](docs/guide/operations.md)自测。MySQL 其他版本、Linux 发行版、云平台、TLS/最小权限等组合须逐项提供实测证据后再列入支持矩阵：`internal/physical/compat.json` 目前只有一条 `status: candidate`、`verified: false` 的记录（MySQL 8.4.11 + PXB 8.4.0-7 + linux/amd64）。
- 备份和恢复会产生受保护的明文工作文件，需要受控磁盘与权限；加密备份仍依赖妥善保管历史密钥。

## 阅读与参与开发

| 你想了解什么 | 文档入口 |
| --- | --- |
| 当前总路线与 Web 推进阶段 | [Web 优先总路线](docs/roadmap/Web优先与多应用架构-2026-10-17.md) |
| 备份恢复设计与验收边界 | [详细能力方案](docs/roadmap/数据库备份与恢复-2026-10-17.md) |
| 全部核心约束目录 | [docs/README.md](docs/README.md) |
| 配置字段 | [app/cli/example.yaml](app/cli/example.yaml) |
| 任务和开发进度 | [GitHub Issues](https://github.com/flyingEirc/Intern/issues)；Web 总路线 [#10](https://github.com/flyingEirc/Intern/issues/10) |
| 如何部署、值守与排障 | [运维使用说明](docs/guide/operations.md) |
| 如何开发与验证 | [开发规范](docs/guide/development.md)、[质量规范](docs/guide/quality.md) |
| 如何跟踪交付和提交 PR | [Issue 流程](docs/workflow/issue.md)、[PR 流程](docs/workflow/pr.md) |
| AI 协作阅读入口 | [AGENTS.md](AGENTS.md)、[CLAUDE.md](CLAUDE.md) |
