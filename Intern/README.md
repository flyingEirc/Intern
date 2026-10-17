# Intern

**为 MySQL 提供可验证的备份与隔离恢复。**

Intern 是一个正在开发的数据库备份与恢复工具，帮助你建立全量备份、归档后续变化，并在需要时恢复到已保存的检查点。当前交付以 CLI 为主，适用于单机、本地文件系统和单个业务库；Web 产品与物理备份能力按路线逐步推进。

## 当前能做什么

| 能力 | 当前行为 |
| --- | --- |
| 逻辑全量备份 | 通过官方 `mysqldump` 导出 InnoDB 一致性快照，包含视图、存储程序、触发器和事件。全量期间须避免并发 DDL。 |
| binlog 增量 | 轮转并归档关闭的 binlog 文件，在全量基线后形成可追溯的增量链。 |
| 检查点恢复 | 选择全量或增量备份 ID，校验完整链后生成恢复计划；显式执行时恢复到隔离目标。 |
| 定时备份 | 每日指定时间全量、按间隔归档增量，支持 IANA 时区；前台常驻运行。 |
| 压缩与加密 | 可配置 Zstandard 压缩和 Tink AES-128-GCM 认证加密，密钥独立保存。 |
| 配置与自动化 | YAML、环境变量、命令行 flags；结果以 JSON 输出，状态与目录索引存入本地 SQLite。 |

已有验证记录覆盖 macOS arm64、MySQL 与官方客户端 9.7.1 下的真实原生进程备份恢复。该记录不代表所有版本、平台或生产环境已验收；详见 [路线第 14 节](docs/roadmap/数据库备份与恢复-2026-10-17.md)。真实容器矩阵和持续质量门禁由 [#5](https://github.com/flyingEirc/IotProject/issues/5) 及其子任务跟踪，是否通过以对应提交与实际 CI 证据为准。

## 从源码开始

准备 mise，以及与源/目标 MySQL 匹配的官方 `mysql`、`mysqldump`、`mysqlbinlog`。工具准入不等于该版本已完成恢复验证。源库需要 binlog、ROW 格式和对应备份/日志读取/轮转权限；当前已记录的恢复验证使用 GTID ON。

Go `1.27.0`、golangci-lint `2.13.2` 和开发任务统一声明在 [mise.toml](mise.toml) 中，Go 版本与 `go.work` 保持一致。首次使用时阅读配置后信任并安装工具。

在本项目目录 `Intern/` 执行：

```sh
mise trust
mise install
mise run build
./bin/intern --help
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

- 当前增量是 binlog 归档；XtraBackup 物理全量/物理增量仍在路线中。
- 当前按已封存检查点恢复；任意时间输入、实例级 PITR、跨库事务恢复尚不在已交付能力中。
- Web、S3、自动保留清理和持续断线归档尚未交付；MySQL 其他版本、Linux、云平台、TLS/最小权限等组合须逐项提供实测证据后再列入支持矩阵。
- 备份和恢复会产生受保护的明文工作文件，需要受控磁盘与权限；加密备份仍依赖妥善保管历史密钥。

## 阅读与参与开发

| 你想了解什么 | 文档入口 |
| --- | --- |
| 产品路线、设计决策和验收边界 | [产品路线与 PRD](docs/roadmap/数据库备份与恢复-2026-10-17.md) |
| 全部核心约束目录 | [docs/README.md](docs/README.md) |
| 配置字段 | [app/cli/example.yaml](app/cli/example.yaml) |
| 任务和开发进度 | [GitHub Issues](https://github.com/flyingEirc/IotProject/issues) |
| 如何开发与验证 | [开发规范](docs/guide/development.md)、[质量规范](docs/guide/quality.md) |
| 如何跟踪交付和提交 PR | [Issue 流程](docs/workflow/issue.md)、[PR 流程](docs/workflow/pr.md) |
| AI 协作阅读入口 | [AGENTS.md](AGENTS.md)、[CLAUDE.md](CLAUDE.md) |
