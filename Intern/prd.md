# Intern 产品需求与整体方案（PRD）

本文件是 Intern 产品范围、技术选型、整体设计与验收要求的统一依据；开发协作与 Issue 追踪规范见 [AGENTS.md](AGENTS.md)。第 1–13 节保留整体方案与原调研边界；本次 CLI 逻辑链的实际实现、使用方式及验证证据见第 14 节，不代表整个产品或所有版本已通过生产验收。

## 项目定位与当前范围

- 仓库名为 **Intern**，采用 Go Workspace（`go.work`）组织多模块、多产品开发。
- 核心产品是数据库备份与恢复工具，目标同时支持全量备份、增量备份和时间点恢复（PITR），当前优先支持 MySQL。
- 当前先开发 CLI 与 Web；不要提前实现尚未确定的移动端或自行扩展数据库支持范围。
- Go 是核心业务与后端的主要语言。当前 `go.work` 声明的目标版本为 `1.27.0`。

## 目录与职责边界

| 目录 | 职责 |
| --- | --- |
| `base/` | CLI、Web 可能共同使用的 Go 基础组件与共享业务能力。 |
| `cli/` | 命令行产品及其专属功能、交互与入口。 |
| `web/` | Web 产品及其后端；前端技术栈待定。 |
| `site/` | 个人产品站点网站，不等同于 Web 产品。 |
| `mobile/` | 可能的后续移动端，当前不作实现承诺。 |

- 跨产品复用的 Go 能力优先放入 `base/`，避免 CLI 与 Web 各复制一套核心业务。
- `base/` 不依赖 `cli/` 或 `web/` 的产品入口；产品专属逻辑留在对应目录。
- 使用 `go.work` 管理实际存在的模块；新增模块时维护相应 `go.mod` 与工作区配置，不为空目录虚构模块。

## 方案约束与阅读指引

- 已确定的技术选型必须遵守；“推荐”“暂定”“待确认”和“未实测”不得解释为已采纳、已实现或已验证。
- 第 1–4 节定义备份能力与两条恢复链；第 5 节定义压缩、加密与密钥；第 6 节集中维护技术选型和共享架构；第 7 节定义状态与保留语义。
- 第 8–10 节记录参考实现、云平台和许可限制；第 11 节保留实际验证证据与边界；第 12 节定义产品验收门禁；第 13 节保留资料来源。
- CLI 与 Web 后端均以 Cobra 构建命令与启动入口，Gin 承担 HTTP 框架职责；Cobra 不代替 HTTP 框架。
- 数据访问使用 sqlc，以 SQL 和生成的类型安全 Go 代码为准；sqlc 不是传统运行时 ORM，不另行引入一套 ORM。
- Web 前端技术栈尚未确定，不擅自将任何前端框架定为标准；本项目选型不要求改写第三方依赖内部采用的技术。
- 加密固定为 AES-128-GCM，不静默替换为 AES-256、CTR 或其他算法/模式。同一密钥下 GCM nonce 必须唯一；使用成熟流式方案检测截断、篡改、块顺序与完整性。
- 压缩固定为 Zstandard（zstd），先压缩后加密，恢复时先认证解密再解压。密钥与备份分离，明文密钥和数据库密码不得进入日志或备份清单。
- 核心设计可参考 Databasus，但其规范不自动成为 Intern 的规范；当前实际路径为仓库根目录的 `../vendor/databasus/`，布局变化时先定位，不擅自搬迁参考代码。

## 1. 结论与决策边界

1. **保留两条独立备份链**：通用链为 `mysqldump 逻辑全量 + mysqlbinlog 连续归档 + PITR`；物理链为 `XtraBackup 物理全量 + 物理增量 + binlog + PITR`。物理增量属于核心目标，不能用“已支持 binlog”替代其交付。
2. **逻辑 dump 不能接 XtraBackup 物理增量**。物理增量依赖 InnoDB 页和 LSN，不是对 SQL dump 作差分。redo log 用于物理备份一致化；binlog 用于基线之后的事务重放。[M1][P1]
3. **加密固定 AES-128-GCM，压缩固定 Zstandard**，机器标识分别为 `aes-128-gcm`、`zstd`。需求中的 `zstand` 指 Zstandard，不另立一种格式。
4. **推荐 Tink Streaming AEAD + klauspost/compress/zstd**，不复制 Databasus 的自研加密容器，不自研 redo/binlog 协议、压缩器或上传分片协议。[G1][G2]
5. 物理引擎优先验证 **MySQL 8.4 + 对应 XtraBackup 8.4 稳定版本 + Linux 数据目录访问**。默认拒绝未列入兼容矩阵的组合，不能关闭版本检查碰碰运气。[P2]
6. Databasus 可参考其业务编排和存储提交模型；它的 MySQL 是逻辑备份，不是现成的 MySQL PITR 实现。[D1]
7. 本次已实际跑通 **MySQL 9.7.1 逻辑备份、压缩加密往返、远程协议 binlog 归档及事务边界恢复**。这不证明 MySQL 8.4、XtraBackup、云数据库或连续断线恢复已通过实测。证据见第 11 节。

下文“推荐”是调研选型，不改变用户已指定的 Go/Cobra/sqlc/goose/slog/Gin/cron/Viper/jsonv2 技术栈。原调研没有添加产品依赖；本次 CLI 采用的组件、版本及单机部署模式已在第 14 节固化，未创建或切换分支。

## 2. 能力模型：全量、增量、PITR 分开表达

| 能力 | 工具 | 基线/产物 | 部署前提 | 恢复路径 |
| --- | --- | --- | --- | --- |
| 逻辑全量 | mysqldump | SQL dump；明确所含库、表、存储程序等范围 | 网络或 socket 访问、所需数据库权限 | mysql 导入 |
| 日志增量归档 | mysqlbinlog | 原始 binlog 及文件/位置、来源标识 | binlog 开启、复制读取权限、足够保留窗口 | 基线后按事务重放 |
| 物理全量 | XtraBackup | 数据文件、redo、副本元数据 | 受支持 Linux 环境、源数据目录访问、数据库备份权限 | prepare、copy-back、启动 |
| 物理增量 | XtraBackup | 页面变化、from_lsn/to_lsn、父备份引用 | 匹配的物理基线和连续链 | 合并增量后恢复 |
| PITR | 基线工具 + mysqlbinlog | 基线与目标之间无缺口的事务日志 | 可恢复基线、完整日志、兼容目标环境 | 恢复到明确事务边界 |

建议备份类型使用 `logical-full`、`physical-full`、`physical-incremental`、`binlog`。API、CLI 和 Web 不应只给一个含糊的 `incremental=true`。

“全量”必须限定范围：一个业务库的全部对象，不等于整个 MySQL 实例及账号、授权、配置都已保护。实例级 PITR 要覆盖相关业务库和跨库事务；带排除表的部分备份不能宣称完整实例 PITR。

## 3. 逻辑全量与 binlog PITR

### 3.1 一致性快照

推荐参数基线：

```text
mysqldump --single-transaction --source-data=2 --quick
          --routines --events --triggers --hex-blob --no-tablespaces
          --databases <明确的数据库集合>
```

这是参数说明，不是可直接运行的产品命令；连接、认证、TLS、版本适配与 GTID 策略须由运行器明确提供。

- `--single-transaction` 保证 InnoDB 的一致性读，不保证 MyISAM/MEMORY 等非事务表的一致性。
- 与 `--source-data=2` 配合，从 dump 内获得与快照协调的 binlog 文件及位置。可能需要短暂全局读锁，不能宣传成无锁备份。
- 不允许在备份完成后随意查一次 `SHOW BINARY LOG STATUS`，就把所得位置当成快照起点；这会漏掉备份期间的事务。
- 备份期间的 ALTER/CREATE/DROP/RENAME/TRUNCATE 等 DDL 可能破坏一致性或使导出失败。采用 DDL 变更窗口控制；不能仅凭进程退出码认定所有并发 DDL 都安全。
- `--flush-logs` 可用于协调轮转，但不是每次备份的强制选项；需要相应权限并评估源库影响。
- 显式包含 routines/events/triggers。权限不足时报告缺失对象和备份范围，不能静默跳过后仍宣称“完整备份”。
- `--no-tablespaces` 减少权限需求，但不意味着账号、授权、外部文件和实例配置自动被导出。[M1]

### 3.2 GTID 策略

**文件 + 位置是基础恢复锚点；GTID 是额外的来源和事务集合证据，不能草率互换。**

- 记录 `server_uuid`、备份范围、GTID 模式、工具版本及工具提供的快照相关元数据；快照之外采集的 GTID 状态只能标记为观测值，不能冒充快照 GTID。
- 本次实验用 `--set-gtid-purged=OFF`，在独立空目标恢复单业务库，再按文件位置重放；没有执行 `SET gtid_purged`，没有验证复制拓扑重建。
- 实例克隆与复制接入如需初始化 GTID，应采用对应版本官方流程，明确快照 GTID 集合、目标现有集合及导入是否记 binlog；不能把上述实验参数直接当作所有场景的策略。
- 部分库 dump 的 GTID 集合可能包含未导出库的事务；禁止无条件把整实例的 GTID 集合写入部分库目标。
- 不默认使用 `--skip-gtids`，不清空目标 GTID 历史以掩盖重复或冲突。[M1][M2]

### 3.3 连续归档与重连

采用 `mysqlbinlog --read-from-remote-server --raw --stop-never`，明确独立 `connection-server-id`、TLS 校验和凭据来源。`--stop-never` 跟随轮转，**不保证断线后自动重连**；Intern 负责进程生命周期，不重新实现复制协议。[M2]

推荐先采用可审计的整文件归档：

1. 每个来源/采集会话使用独立、权限受限的 spool 目录。`--raw` 会覆盖同名输出，不能拿正式归档目录当续写目录。
2. 断线后从仍在源端的受影响文件重新抓取到新目录；验证完整性后按来源、文件名、有效范围去重。不要未经验证地拼接半个事件。
3. 已关闭文件通过 binlog checksum 验证，再流式 zstd + AES-128-GCM 加密并上传；成功发布前不推进持久归档水位。
4. 活跃文件的本地缓冲不能当成异地已保护数据。若整文件轮转间隔不满足 RPO，需要有权限的周期性 `FLUSH BINARY LOGS`，或另行验证按完整事务分段的成熟组件方案；不能在指标上伪装低 RPO。
5. 源端已 purge 所需文件时，将恢复窗口截断在最后连续点并报警；建立新的全量基线。禁止跳过缺失日志继续宣称连续 PITR。
6. 来源发生故障切换/重建/日志编号复用时开启新的来源世代，检查 UUID/GTID 关联；文件名相同不是同一日志。

`--raw` 的 spool 可能含明文，即使源端启用了 binlog encryption，远程导出的日志也不等同于已加密归档。spool 必须使用受保护的磁盘/目录、最小权限与回收策略；“最终备份已加密”不能被写成“全链路无明文落盘”。若要求完全禁止明文磁盘，应使用受控 tmpfs/加密卷并对容量和故障行为单独验收。[M2]

### 3.4 时间点与恢复执行

- API 接收带时区的目标时间，内部保存 UTC，同时呈现解析后的文件/位置与事务边界。
- datetime 选项用于查找事件；执行采用经过确认的 `start-position` / `stop-position`，不依赖秒级时间筛选承担精确边界语义。mysqlbinlog 的 datetime 解释受运行机时区影响。[M3]
- 目标必须落在完整事务之间。不能把一个事务的一部分当成已恢复；同秒多个事务也不能只按显示时间猜测。
- 多文件在有序、连续校验后，通过同一个 mysql 会话重放，避免临时表与会话状态丢失；按需要启用 `--binary-mode`，不得以展示用的 `DECODE-ROWS` 输出作为可执行恢复 SQL。[M2][M3]
- 默认恢复到新实例/隔离目标。恢复中断可能已经执行部分 SQL，不能声称回滚到恢复前；失败目标隔离，重新制定恢复计划。
- 在导入或启动业务前验证备份全流认证、日志范围与文件摘要。若选择边验证边导入，必须只向可丢弃的隔离目标写入，最终成功前不得开放业务。

## 4. XtraBackup 物理全量、增量与 PITR

### 4.1 版本和部署准入

以下是检索时的官方文档状态，不是永远有效的版本白名单：

| 来源 | 文档事实 | Intern 建议 |
| --- | --- | --- |
| MySQL/Percona Server 8.4 | PXB 8.4 文档列出 8.4.0-7；明确不支持 MySQL 8.0/9.x | 物理能力优先验收组合；固定补丁版本和镜像摘要 |
| 8.0 系列 | PXB 8.0 文档存在 8.0.35-36，页首有 EOL/只读文档告警 | 有工具不代表当前产品已支持；作为存量兼容单独评估，不作为新部署默认 |
| MySQL 9.7 | 检索到的 PXB 9.7 发布索引只有 9.7.1-rc1 | 不作为生产物理引擎默认；本机 9.7.1 逻辑实验不为该组合背书 |
| 其他版本/发行版 | 没有本报告验证过的对应组合 | 默认关闭物理能力；依据官方支持声明和恢复演练加入矩阵 |

不能仅比较版本字符串前缀：还要核实源服务器、备份工具、prepare 工具、恢复目标、存储引擎、升级历史及发行版兼容性。禁止用 `--no-server-version-check` 绕过门禁。[P2]

物理 worker 必须能访问源实例对应的数据文件，通常部署在源 Linux 主机或正确挂载卷的容器中；仅能连接远程 MySQL TCP 地址不足以备份物理数据。Web 控制面无需获得所有源机器的 root 权限。权限至少按所用版本核实 BACKUP_ADMIN、PROCESS、RELOAD、LOCK TABLES、REPLICATION CLIENT 及所需 performance_schema 表读取权限；不直接照搬一条跨版本 GRANT。[P3]

### 4.2 完整备份链

- 每条链从 XtraBackup 物理全量开始；增量记录父备份 ID、`from_lsn`、`to_lsn`、`last_lsn`。
- `from_lsn == 父备份 to_lsn` 是必要条件，不是全部充分条件：还要核对来源世代、实例和工具版本、链 ID、文件完整性。
- 保存工具原始 `xtrabackup_checkpoints`、`xtrabackup_info`、`backup-my.cnf`、`xtrabackup_binlog_info` 以及该版本实际生成的 GTID/表空间元数据；不能硬编码所有版本都会生成相同文件。
- 为降低首次实现复杂度，推荐先保留完整的原生备份目录，用标准归档容器或成熟 xbstream 工具收集，再走统一压缩加密层。临时磁盘必须容纳备份目录；后续如切为 xbstream 直传，需重新验证检查点持久化与失败收尾，不自研流格式。
- 不同时启用两套冗余压缩/备份加密。PXB 的某个选项名包含 AES，并不证明它就是本项目要求的 AES-128-GCM；本项目统一使用已验证的 Tink 外层。
- InnoDB 页增量不是整个实例所有文件都只复制变化页：非 InnoDB 引擎可能要全量拷贝并加锁。[P1][P2]

### 4.3 prepare 与恢复

以下仅为官方流程模板，**本次未执行 XtraBackup**。每次恢复使用从不可变归档解出的新工作副本，不在唯一备份上原地 prepare：

```text
xtrabackup --prepare --apply-log-only --target-dir=<work/base>
xtrabackup --prepare --apply-log-only --target-dir=<work/base> --incremental-dir=<work/inc1>
xtrabackup --prepare                  --target-dir=<work/base> --incremental-dir=<work/inc2>
```

- 全量与中间增量使用 `--apply-log-only`，最后一环完成 rollback；提前 rollback 会破坏继续合并后续增量的能力。
- 同一个增量工作目录不要用于重复 prepare；重新从不可变归档展开一份。
- 无增量的物理全量直接按普通 prepare 流程处理。
- `copy-back` 面向已停库的空目标 datadir；检查属主、文件权限、配置、磁盘空间后启动。
- 从**选中的最终物理恢复点**对应的 binlog 坐标继续回放，不能仍使用最初全量的坐标重复执行已合并事务。
- 目标时间早于某个增量覆盖点时不能应用该增量；应选择不晚于目标的可用基线/增量前缀。时刻不能仅按备份结束时间猜测，必须结合工具恢复坐标和事务日志。[P1][P4]

### 4.4 redo 与表空间加密

- redo archiving / redo consumer 是避免长备份期间未复制的 redo 被覆盖的机制，不是替代 binlog 的通用 PITR 引擎。容量不足仍会影响备份或源库运行，不能无限保留而不设资源上限。[P5]
- InnoDB 表空间加密与 Intern 备份加密独立。恢复需要历史 keyring/对应组件配置或官方支持的 transition-key 流程；外层 AES-128-GCM 解密成功不表示 InnoDB 能打开表空间。
- 官方说明 XtraBackup 不自动把 keyring_file 复制进备份目录。必须独立保护和保留所需历史密钥；不能轮换后删除仍被备份引用的 keyring。密钥缺失时拒绝宣称物理备份可恢复。[P6]

## 5. 压缩、流式加密、密钥与文件完整性

### 5.1 组件选择

| 用途 | 推荐组件 | 依据与取舍 |
| --- | --- | --- |
| Zstandard | `github.com/klauspost/compress/zstd` | 纯 Go，支持 io.Writer/io.Reader；本次验证版本 v1.20.0 |
| 流式 AES-128-GCM | `github.com/tink-crypto/tink-go/v2/streamingaead` | 采用 `AES128GCMHKDF1MBKeyTemplate()`；本次验证版本 v2.8.0 |
| 小型密钥/清单认证加密 | Tink AEAD / encrypted keyset API | 复用成熟组件，不另造密钥包装格式 |

Tink 模板的主密钥和派生 AES 密钥均为 16 字节，采用 HKDF-SHA256 派生和 1 MiB 密文段。HKDF 是流式方案的密钥派生，不是把算法改成 AES-256。格式应同时记录 `cipher=aes-128-gcm` 与具体容器/模板标识，不能把 Tink 流与“一次 Seal 的裸 GCM 密文”混称为可互换格式。[G1]

### 5.2 数据管道与提交

```text
工具输出 → 有界流式拷贝 → zstd → Tink Streaming AEAD → 临时对象/上传器
                                                           ↓
全部读写/Close/工具退出成功 → 发布认证清单 → 元数据事务标记 completed
```

- 顺序固定先压缩后加密；关闭顺序为 zstd、加密 writer、存储 writer，全部检查错误。工具失败、短写、上传失败、Close 失败或取消都不能发布 completed。
- 不整库 `ReadAll`；限制拷贝缓冲、压缩并发、解压内存、展开大小和任务并发。第 11 节实验为了构造损坏样本使用内存缓冲，不代表生产实现应照搬。
- Tink 负责 nonce、块顺序和终止认证；必须读到经过认证的 EOF，不能只解出第一块就认为完整。
- 网络中断后不向旧加密流原地追加不同内容；重新创建流/对象。对象存储可恢复上传仅针对相同已生成字节，不意味着能任意续写压缩加密状态。
- 已封存备份不可变；prepare、恢复和重试都使用新工作副本。

### 5.3 AAD、清单和密钥

- AAD 绑定不可变 `format_version`、`project_id`、`source_id`、`artifact_id`、对象角色和 codec/模板标识，防止跨项目或跨对象替换。不要绑定可变展示名称。
- AAD 使用明确编码的原始字节并保存；不能依赖 JSON map 的随机遍历顺序重新序列化得到“相同”AAD。持久化原字节优于自行声称实现了通用 canonical JSON。
- 发布清单记录全部对象、字节数、SHA-256、顺序及备份链关系，并用成熟 AEAD 组件认证保护。SHA-256 用于完整性核对，不能替代密码认证。
- 使用 Tink encrypted keyset 机制；数据密钥随机生成，由独立 KEK 保护。CLI 可从受保护的密钥文件/操作系统 secret 机制获取 KEK，部署环境可用 KMS；禁止用 UUID、用户名或普通配置字符串直接充当 AES 密钥。
- 明文密钥不得存入备份桶或日志。加密后的 keyset 可以随恢复材料保存，但解封 KEK 必须独立保护，具备离线/灾备恢复路径。
- 轮换密钥保留旧备份所需的 key ID 与解密能力；错误密钥、被篡改元数据、缺失 keyring 都应在实际恢复前失败。[G1][G3]

## 6. Go 组件与共享架构

### 6.1 选型表

| 领域 | 已定/推荐 | 落地约束 |
| --- | --- | --- |
| 入口 | 已定 Cobra | CLI 命令与 Web `serve` 启动入口；不在 Cobra 回调里堆核心业务 |
| HTTP | 已定 Gin | Handler 只负责协议、鉴权、参数和任务提交 |
| SQL | 已定 sqlc + database/sql | 元数据 SQL 生成；不是用 sqlc 重新实现数据库 dump |
| Migration | 已定 goose | 使用库模式与嵌入迁移，避免要求用户额外安装 goose 可执行文件 |
| 日志 | 已定 slog | 接入 cron/工具日志时脱敏；记录任务/来源 ID，不记录凭据或完整敏感 SQL |
| 配置 | 已定 Viper | flags/env/file/default 优先级；推荐只保存 secret 引用。本次依用户明确要求兼容 `chmod 600` YAML 中的 password，禁止写入日志、清单或复制到备份目录。 |
| JSON | 已定 Go 1.27 `encoding/json/v2` | 本机 Go 1.27.0 无额外 GOEXPERIMENT 即可编译并完成往返 |
| 调度 | 推荐 `github.com/robfig/cron/v3` | 标准五字段与显式时区；cron 只提供触发，不承担持久任务队列/跨进程锁 |
| MySQL 驱动 | 推荐 `github.com/go-sql-driver/mysql` | 连接探测、元数据查询；备份/回放仍交给官方工具 |
| 压缩/加密 | 推荐 klauspost zstd + Tink | 见第 5 节，禁止临时手写分块协议 |
| 对象存储 | 推荐 `gocloud.dev/blob` 的 fileblob/s3blob | 已有统一 io 接口与取消/Close 语义；只导入所需 provider，不复制 rclone 全栈 |

Go CDK writer 的 Close 是提交点，失败路径先取消写入 context 再 Close 释放资源；仍须在业务层区分“对象存在”与“备份已发布”。本地目录与 S3 的持久性、原子性不能假定完全相同。[G4]

**Gin/jsonv2 陷阱**：核查的 Gin 默认 codec 仍导入 `encoding/json`。只把 Go 升级到 1.27，不等于所有 `ShouldBindJSON`/`c.JSON` 都采用 jsonv2 新语义。推荐在自有请求/响应边界直接使用 `encoding/json/v2` 编解码并通过 Gin 输出字节，或基于选定 Gin 版本提供的 codec 接口统一适配；不要 fork Gin，也不要假定存在未核实的 build tag。[G5]

### 6.2 元数据存储建议

目标 MySQL 是被备份对象，**不自动意味着 Intern 的任务目录必须存进同一个 MySQL 实例**。

- 单机 CLI/单实例 Web：推荐 SQLite + `modernc.org/sqlite`，免外部服务，元数据目录与备份数据分离。
- 多实例服务：推荐独立 MySQL 元数据库，由 sqlc 生成对应查询；不与被保护的源库共享故障域。
- 不同时预造两套后端。首个部署模式选定后只实现需要的一套；扩大部署范围时显式迁移。
- 风险：sqlc 官方矩阵把 Go/MySQL 标记为 Stable、Go/SQLite 标记为 Beta。若选 SQLite，模式冻结前应运行实际 SQL 的生成、goose 迁移和事务并发验证；本次没有将 SQLite/sqlc 组合标成实测通过。[G6]

### 6.3 职责与分支

```text
cli/：Cobra + 终端交互 ─┐
                      ├→ base/：备份/恢复用例、任务、存储、密钥、元数据
web/：Cobra + Gin ─────┘                    ↓
                              mysqldump/mysqlbinlog/XtraBackup 运行器
site/：个人产品网站；不承担备份控制面
mobile/：未来可能的客户端；当前不实施
```

- Go Workspace 保留 `base/`、`cli/`、`web/` 模块边界；base 不导入产品入口，目录按实际职责形成，不先建立空抽象。
- CLI/Web 各自分支开发，共用能力进入 base，通过明确接口协调合入；不各写一套恢复引擎。
- 单机模式由一个明确的调度所有者运行持续任务；一次性 CLI 命令退出后不能声称还在归档 binlog。CLI 常驻模式与 Web 服务模式共享同一任务引擎，但不能同时无锁消费同一工作目录。
- 多项目场景中任务、数据源、存储前缀、清单和密钥引用都携带 project ID；Web 必须在调用共享用例前做项目权限校验。
- 恢复由显式计划驱动：目标身份、覆盖风险、备份/日志清单、停止事务、所需空间与密钥全部列出，执行前验证目标不是源生产实例。

## 7. 状态、提交、保留与可恢复窗口

### 7.1 最小元数据

| 实体 | 必需信息 |
| --- | --- |
| Source | project ID、来源 ID/世代、server UUID、版本、引擎、GTID/binlog 能力、凭据引用 |
| Backup | 类型、状态、范围、父/基线 ID、工具版本、快照 binlog 坐标、LSN、认证清单引用 |
| Artifact | 对象键、角色、格式、大小、摘要、加密模板、key ID、发布状态 |
| Binlog archive | 来源世代、文件/有效位置范围、GTID 信息（如有）、校验状态、持久归档水位 |
| Job/Restore | 请求/调度幂等键、状态、所有者、取消请求、错误分类、恢复计划及验证结果 |
| Pending deletion | 待删除对象、原因、引用/代次、清理状态 |

建议任务状态为 `queued → running → finalizing → completed`，异常进入 `failed/canceled`。恢复验证结果单独记录，不能让 completed 自动代表“恢复演练成功”。

### 7.2 提交与失败边界

- 对象写入前记录暂存/清理责任；只有对象和清单都成功、元数据事务提交后，备份才对恢复选择器可见。
- 进程崩溃在“上传完成、事务未提交”之间产生孤儿对象，由清理账本回收；不能依赖正常退出时的 defer。
- 活跃写者需要持久租约/代次或单进程所有权保护，清理器不得删除仍在上传的文件。若采用多 worker，发布和清理必须通过条件更新/事务认领互斥，不能只靠进程内 map。
- cron 触发以 `(schedule_id, scheduled_at)` 幂等；手工请求有独立幂等键。单源并发限制与恢复目标排他锁须覆盖 CLI/Web 两个入口。
- 停机/取消关闭工具和管道、等待进程退出并记录失败；不能把取消造成的 EOF 当成正常完成。

### 7.3 保留策略

- 清理按依赖图，不按文件年龄孤立删除：被保留增量引用的全量/中间增量不能删除，正在恢复的对象必须固定引用。
- PITR 保留窗口必须包含窗口起点之前的可用基线，以及从其恢复坐标到窗口终点的全部日志。只保留最近 N 天 binlog 而删除更早基线会破坏窗口。
- 可恢复水位由**连续、已认证、已持久发布**的日志确定，不是采集进程看到的最新时间，也不是对象修改时间。
- RPO 取决于最后已保护事务与当前源状态的差距；RTO 包含下载、解密解压、prepare/导入、日志重放、验证时间。本次不承诺具体秒数或吞吐量。

## 8. Databasus：参考什么、不复制什么

源码路径以下均相对于本次工作目录的 `vendor/databasus/`，从 Intern 仓库根访问为 `../vendor/databasus/`。

| 核查项 | 直接证据 | 结论 |
| --- | --- | --- |
| MySQL 能力 | `README.md:114-117` | MySQL 明确 logical only；不能用 PostgreSQL WAL/PITR 代码为 MySQL 能力背书 |
| dump 参数 | `backend/internal/features/backups/backups/usecases/logical/mysql/create_backup_uc.go:126-174` | 有 single-transaction、GTID OFF 等；没有可直接沿用的完整 PITR 位点链 |
| 流式备份 | 同文件 `:234-321` | 参考 zstd→加密→上传的结构，但必须完整传播 Close/进程/上传错误；不要照搬固定缓冲与压缩级别当成最优值 |
| 密钥长度 | `backend/internal/features/backups/backups/encryption/utils.go:12-35` | PBKDF2 输出 32 字节，现有实现是 AES-256-GCM；不符合本项目约束 |
| 流终止 | `backend/internal/features/backups/backups/encryption/decrypting_reader.go:70-88` | 加密读取层把块边界 EOF 当作结束；未见认证的最终块标记。不能仅改密钥长度就声称得到完整安全流格式；此处为源码分析，未运行该库的攻击实验 |
| 发布/删除 | `backend/internal/features/storages/files/store.go:45-171`、`repository.go`、`worker.go` | 值得借鉴“先登记清理责任、提交事务认领”的思想；不要照搬 PostgreSQL 专属 SQL/GORM 模型 |
| 调度/恢复 | `backend/internal/features/backups/backups/backuping/logical/scheduler.go:107-158,304-352` | 参考失败恢复；先查 IN_PROGRESS 再启动不等于跨实例原子互斥 |
| 恢复错误 | `backend/internal/features/restores/core/stream_failure.go` | 数据流失败可能留下部分恢复目标；Intern 采用隔离目标和最终开放门禁 |

不引入 Databasus 的 GORM、另一套配置库、全局单例装配或自研加密容器。参考实现不是项目规范，也不是每个安全边界都已由我们验证过的组件。

## 9. 云托管与替代工具

### 9.1 云托管按实际能力分类

- **无数据目录访问时不可运行本产品的 XtraBackup 源端物理备份**；托管平台自己的物理备份不是同一个接口。
- RDS 官方支持 mysqlbinlog 下载/流式读取，但必须核实 binlog retention hours、权限和网络；不能沿用自建 MySQL 的默认保留假设。[C1]
- Azure Flexible Server 有平台备份/PITR 和平台托管的日志生命周期；外部归档必须核实当前参数和账号能力，不能把平台备份当成可下载的原生 XtraBackup 文件。[C2]
- Cloud SQL PITR 创建新实例；文档对 Cloud Storage 中的平台归档日志访问有专门限制。**该限制不能推导为所有复制协议读 binlog 都不可用**。Intern 应区分“平台 PITR”“在线实例外部日志读取”“可导出的归档”，逐项能力探测，不宣传通用云兼容。[C3]
- 云环境无权限取快照坐标或持续日志时，显式报告仅逻辑备份/平台 PITR 能力，不能在界面显示自主连续 PITR 已启用。平台 API 编排不在本次已验证范围内。

### 9.2 替代方案选择

| 方案 | 优势 | 本次结论 |
| --- | --- | --- |
| mysqldump/mysqlbinlog | 官方工具、格式与恢复路径清晰、可远程执行 | 通用默认 |
| MySQL Shell dump/load | 成熟并行导出/分块加载，适合逻辑大库 | 若测得 mysqldump 无法满足 RTO，再用它替换相应适配器；不是物理增量 |
| MyDumper/MyLoader | 成熟开源并行逻辑备份/恢复 | 可比较性能，不先同时维护第二套格式；不是物理增量 |
| XtraBackup | 物理全量与页面增量 | 自建 Linux 且版本/权限匹配时采用 |
| MySQL Enterprise Backup | 商业物理备份产品 | 不作为开源默认依赖 |
| 自研 redo/复制协议 | 无已证明的必要性 | 拒绝；使用已有工具或成熟协议库 |

binlog 结构索引若确实需要程序化解析，再评估成熟 MySQL 协议库；首次实现可使用官方工具作日志验证、定位与恢复，不能用正则解析业务 SQL 来猜事务含义。[M1][M2][A1]

## 10. 许可与发布约束

- Databasus 代码 Apache-2.0；复制代码须保留适用版权/NOTICE/修改说明。Databasus 名称和标志不是 Intern 的可复用品牌。[D2]
- Tink Apache-2.0；klauspost/compress 的 zstd 部分采用 Go/BSD 风格许可证，其仓库其他子目录还有不同许可，不能只按仓库首页的一个标签判断全部依赖。[G1][G2]
- MySQL Community 工具、XtraBackup、所查 MySQL Shell 8.4 分发包含 GPLv2 许可；MyDumper 为 GPLv3。独立进程调用与链接/复制代码需要分别审查，不能给出“调用 CLI 就完全没有义务”的保证。[L1]
- 若发布镜像内携带这些二进制，必须提供对应许可证、适用源码/源码提供安排和第三方声明；初期可显式要求安装经验证的工具版本，不能静默下载未经校验的二进制。
- 所有生产依赖和工具固定版本/校验和；安全更新后重跑真实恢复矩阵。以上是工程发布要求，不替代法律意见。

## 11. 本次实验、输出与验证边界

### 11.1 实际运行环境

- Go：`go version go1.27.0 darwin/arm64`；`GOEXPERIMENT` 为空。
- `go list encoding/json/v2` 成功；实际 Marshal/Unmarshal 往返成功。
- Tink Go v2.8.0、klauspost/compress v1.20.0，仅安装在一次性临时模块。
- MySQL/mysqldump/mysqlbinlog：本机 9.7.1；两个全新 datadir，`--no-defaults`、`--skip-networking`、`--mysqlx=0`，仅 Unix socket，未接触现有实例。
- 源库 InnoDB、ROW、GTID ON；测试帐号为隔离实例的临时 root，不代表已验证最小权限。

### 11.2 压缩与认证加密

对 4 MiB + 37 字节随机输入使用 zstd→Tink AES128GCMHKDF1MB，反向恢复字节完全一致；同时验证空流。

```text
PASS multi-segment roundtrip: plaintext=4194341 ciphertext=4194554
PASS empty stream roundtrip
PASS reject: bit flip
PASS reject: wrong AAD
PASS reject: wrong key
PASS reject: missing final byte
PASS reject: truncate at segment boundary
PASS reject: reordered segments
PASS reject: removed segment
PASS reject: appended bytes
PASS Go jsonv2 roundtrip: {"algorithm":"aes-128-gcm"}
```

这是调用真实库的运行实验，不是 mock 回显，也不是产品吞吐/峰值内存基准。

### 11.3 MySQL 逻辑全量与定点恢复

实际步骤：

1. 源库创建 `intern_probe.records`，写入 `(1, baseline)`。
2. 用 `--single-transaction --source-data=2 --set-gtid-purged=OFF --routines --events --triggers --no-tablespaces --databases intern_probe` 导出，读取 dump 内坐标 `binlog.000001:923`。
3. 提交 `(2, wanted)` 后记录下一个事件边界 `binlog.000001:1220`。
4. 再删除 id=1、写入 `(3, unwanted)`，使源库成为 id=2、3。
5. `FLUSH BINARY LOGS` 后，使用 `mysqlbinlog --read-from-remote-server --raw --to-last-log` 经源实例 socket 协议下载日志，得到 `binlog.000001` 和 `binlog.000002`；不是直接复制源 datadir 文件。
6. SQL dump 经过 zstd+AES-128-GCM 再解密解压，2394 字节恢复一致，密文为 1035 字节。
7. 在第二个全新实例导入恢复后的 SQL，先核对只有 id=1。
8. 对归档的 `binlog.000001` 执行 `mysqlbinlog --start-position=923 --stop-position=1220`，把输出交给目标 mysql 客户端。
9. 目标得到 id=1、2，源库仍是 id=2、3；错误删除及其后的插入未被恢复。

```text
PASS restored encrypted/compressed logical baseline: id=1
PASS PITR at committed transaction boundary: 1 baseline; 2 wanted
PASS unwanted DELETE and later INSERT excluded; source still: 2 wanted; 3 unwanted
```

SQL dump SHA-256：`e556a22cc778ba21a3de449b1898d66b518ff4ac5edbc939890cfda6f7497a02`。

两个隔离 mysqld 均已正常停止。一次性模块、密钥与测试数据不进入仓库。

### 11.4 没有验证的内容

- 没有运行 XtraBackup：本机无该二进制，Docker daemon 未运行，也没有提供可访问的受支持 Linux/PXB 数据目录环境。
- 未验证 MySQL 8.4/8.0 实际恢复、RC 版 PXB、跨版本迁移、云平台、最小权限和 TLS 握手。
- 未验证持续 `--stop-never` 采集的断线重连、源库重启、purge、故障切换、多文件含临时表回放、并发 DDL/大事务。
- binlog 在本次实验中是原始本地归档，未通过与 dump 相同的压缩加密管道；不能把两项独立实验夸大为完整产品归档链已验收。
- 未验证 Tink 密钥持久化/KMS、S3 上传、清理竞态、sqlc+goose 元数据库或 Gin/jsonv2 HTTP 集成；相应选型依据是文档，而非产品实测。

## 12. 实施依赖与产品验收门禁

以下仍是完整产品的实现与发布门禁，不表示所有单元已交付；当前 CLI 逻辑链的局部交付与实测边界见第 14 节：

| 实施单元 | 依赖 | 产品能力成立的证据 |
| --- | --- | --- |
| 共享核心与制品 | base、密钥、存储、任务状态、sqlc/goose | 完整/空对象可恢复，所有失败/取消不发布成功，清理不误删活跃写入 |
| CLI/Web 逻辑全量 | 官方客户端、共同用例 | 两入口对同一备份产生相同恢复语义；真实数据/对象范围验证 |
| 日志归档与 PITR | 快照锚点、持久归档、恢复计划 | 并发 DML、多文件、GTID、断线/轮转/purge/来源切换、边界与缺口拒绝 |
| 物理全量与增量 | Linux、精确版本矩阵、XtraBackup | 全量+至少两个增量+binlog 实际恢复；断链、错误基线、提前 prepare、缺 keyring 均拒绝 |
| 生命周期 | 上述产物依赖模型 | 保留窗口不被清理破坏，正在恢复的对象被保护，旧 key 可恢复 |
| 发布 | 上述能力和许可清单 | 每个宣称支持的版本组合有恢复记录；不把文档推断写成产品支持 |

CLI/Web 可在独立分支并行实现协议层，但先约定 base 的任务、备份类型、清单和恢复计划契约。物理增量不能从验收范围中删除；尚未完成时应明确显示不可用及原因。

## 13. 资料索引

### MySQL / Percona

- [M1] [mysqldump 官方手册](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)、[PITR 概览](https://dev.mysql.com/doc/refman/8.4/en/point-in-time-recovery.html)。
- [M2] [mysqlbinlog 手册](https://dev.mysql.com/doc/refman/8.4/en/mysqlbinlog.html)、[静态和持续 binlog 备份](https://dev.mysql.com/doc/refman/8.4/en/mysqlbinlog-backup.html)。
- [M3] [按事件位置恢复](https://dev.mysql.com/doc/refman/8.4/en/point-in-time-recovery-positions.html)。
- [P1] [创建增量](https://docs.percona.com/percona-xtrabackup/8.4/create-incremental-backup.html)、[准备增量](https://docs.percona.com/percona-xtrabackup/8.4/prepare-incremental-backup.html)。
- [P2] [PXB 8.4 支持范围](https://docs.percona.com/percona-xtrabackup/8.4/index.html)、[8.0 状态](https://docs.percona.com/percona-xtrabackup/8.0/index.html)、[9.7 发布索引](https://docs.percona.com/percona-xtrabackup/9.7/release-notes/release-notes.html)、[版本检查](https://docs.percona.com/percona-xtrabackup/8.4/server-backup-version-comparison.html)。
- [P3] [数据库权限](https://docs.percona.com/percona-xtrabackup/8.4/privileges.html)、[操作系统权限](https://docs.percona.com/percona-xtrabackup/8.4/permissions.html)。
- [P4] [恢复物理备份](https://docs.percona.com/percona-xtrabackup/8.4/restore-a-backup.html)、[物理基线后的 PITR](https://docs.percona.com/percona-xtrabackup/8.4/point-in-time-recovery.html)、[生成文件](https://docs.percona.com/percona-xtrabackup/8.4/generated-files.html)。
- [P5] [Redo log archiving](https://docs.percona.com/percona-xtrabackup/8.4/redo-log-archiving.html)。
- [P6] [加密 InnoDB 表空间备份与恢复](https://docs.percona.com/percona-xtrabackup/8.4/encrypted-innodb-tablespace-backups.html)。

### Go 组件

- [G1] [Tink Streaming AEAD](https://developers.google.com/tink/streaming-aead)、[AES-GCM-HKDF 格式](https://developers.google.com/tink/streaming-aead/aes_gcm_hkdf_streaming)、[Go 模板源码](https://github.com/tink-crypto/tink-go/blob/main/streamingaead/streamingaead_key_templates.go)、[许可证](https://github.com/tink-crypto/tink-go/blob/v2.8.0/LICENSE)。
- [G2] [zstd 流式接口](https://github.com/klauspost/compress/tree/master/zstd)、[许可证](https://github.com/klauspost/compress/blob/v1.20.0/LICENSE)。
- [G3] [Tink 密钥管理](https://developers.google.com/tink/key-management-overview)。
- [G4] [Go CDK Blob 读写与取消](https://gocloud.dev/howto/blob/)、[项目维护与许可信息](https://github.com/google/go-cloud)。
- [G5] [Gin 默认 JSON codec](https://github.com/gin-gonic/gin/blob/master/codec/json/json.go)、[codec 接口](https://github.com/gin-gonic/gin/blob/master/codec/json/api.go)、[Go jsonv2 起源与语义说明](https://go.dev/blog/jsonv2-exp)；Go 1.27 可用性以本次本机编译结果为准，不以 Go 1.25 的实验开关推断。
- [G6] [sqlc 支持矩阵](https://docs.sqlc.dev/en/latest/reference/language-support.html)、[goose](https://github.com/pressly/goose)、[modernc SQLite](https://pkg.go.dev/modernc.org/sqlite)、[MySQL 驱动](https://github.com/go-sql-driver/mysql)、[robfig/cron](https://github.com/robfig/cron)。

### 参考实现、云平台与许可

- [D1] 本地 `vendor/databasus/README.md:114-117` 和第 8 节列出的源码。
- [D2] 本地 `vendor/databasus/LICENSE`、`vendor/databasus/NOTICE.md`。
- [C1] [RDS binlog 访问和保留](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_LogAccess.MySQL.Binarylog.html)。
- [C2] [Azure MySQL 备份恢复](https://learn.microsoft.com/en-us/azure/mysql/flexible-server/concepts-backup-restore)、[服务器参数](https://learn.microsoft.com/en-us/azure/mysql/flexible-server/concepts-server-parameters)。
- [C3] [Cloud SQL PITR](https://docs.cloud.google.com/sql/docs/mysql/backup-recovery/pitr)。
- [A1] [MySQL Shell dump utilities](https://dev.mysql.com/doc/mysql-shell/26.7/en/mysql-shell-utilities-dump-instance-schema.html)、[MyDumper](https://github.com/mydumper/mydumper)、[Enterprise Backup](https://dev.mysql.com/doc/refman/8.4/en/mysql-enterprise-backup.html)。
- [L1] [XtraBackup/MySQL 随附许可](https://github.com/percona/percona-xtrabackup/blob/trunk/LICENSE)、[MySQL Shell 8.4 许可](https://github.com/mysql/mysql-shell/blob/8.4/LICENSE)、[MyDumper 许可](https://github.com/mydumper/mydumper/blob/master/LICENSE)。

## 14. CLI 逻辑备份交付（2026-09-19）

追踪：[父 Issue #1](https://github.com/flyingEirc/IotProject/issues/1)；[MySQL 引擎 #2](https://github.com/flyingEirc/IotProject/issues/2)、[制品与元数据 #3](https://github.com/flyingEirc/IotProject/issues/3)、[配置与调度 #4](https://github.com/flyingEirc/IotProject/issues/4)。

### 14.1 当前实现与决策

- `base/backup`：MySQL 官方工具运行、全量快照、日志增量、恢复计划及执行。`base/artifact`：zstd/Tink 流与 KEK。`base/catalog`：SQLite/goose/sqlc 发布和清理账本。`base/schedule`：单所有者前台调度。`cli/cmd`：Cobra/Viper 参数与交互，不复制共享恢复引擎。
- 部署选定为单机、本地文件系统、单业务库、InnoDB、ROW binlog。逻辑全量使用 `--single-transaction --source-data=2 --routines --events --triggers --set-gtid-purged=OFF`；快照位置来自 dump，而非事后查询。SQL dump 不携带 `CREATE DATABASE/USE`，目标库名由恢复命令指定，保留源字符集和排序规则。
- 用户要求“每隔多久增量一份”，因此本次采用**按间隔轮转并归档关闭的完整 binlog 文件**，不是持续 `--stop-never` 服务。成功归档后才推进到新日志的 position 4。需要 `FLUSH BINARY LOGS` 权限和足够的源日志保留时间。一次性命令退出后无后台归档；长任务期间合并错过的间隔，不并发追赶。
- 调度启动时无基线则先全量，有基线则先增量检查。每天按 `HH:MM + IANA timezone` 触发全量，增量按配置时长的墙钟边界触发；同时到期时全量优先。没有离线补跑承诺。缺日志、来源/GTID 历史变化或不安全 SQL 范围会报警并建立新全量；**旧基线与新基线之间的缺口不会被伪称连续**。
- 源能力检查、官方 checksum/replay 限制检查及成熟 `go-mysql`/TiDB AST 范围检查拒绝不安全日志，不手写 binlog 协议，不用正则重写业务 SQL。源实例全局 GTID 是观测证据，不冒充快照 GTID。
- SQLite 在制品写入前记录 running；临时目录与制品成功封存、Sync/rename 后才提交 completed。只列出 completed 备份；恢复工作目录同样登记清理责任。重新启动在目录独占锁下回收失败/中断的本工具任务目录。不同备份/元数据目录的进程还通过 MySQL 会话锁互斥源和目标操作。
- 数据流使用 Tink `AES128GCMHKDF1MB`；每个制品的数据 keyset 由独立随机 AES-128-GCM KEK 加密。清单绑定项目、来源、制品、范围、链、codec 和 SHA-256。发布元数据记录预期加密模式及 key ID，拒绝密文制品被替换为明文格式；关闭加密时只有摘要校验，不宣称抵御恶意篡改。
- 恢复先完成全链摘要与认证、受限展开、binlog 顺序/范围检查，再创建目标库并在一个 mysql 会话中导入全量及日志。默认只生成计划，`--execute` 才写入。源 UUID 相同、目标非空、已有来源 GTID 历史、GTID 模式不匹配均拒绝。计划明确权限未预检；执行失败可能已经产生部分 SQL 副作用。
- 备份与 state 目录必须分离且为 `0700`，KEK 必须独立且为 `0600`，不能放进这两个目录；`keygen` 同样执行位置检查。备份期间会存在受保护的明文工作文件；恢复认证暂存归属持久清理账本，不承诺完全无明文磁盘。
- KEK 文件不可覆盖生成；旧备份需要保留旧 KEK。支持由 Tink 管理的多键 KEK keyset（旧键保留、新主键），不自制轮换格式；本次没有添加 KMS 或独立轮换命令。

固定依赖见 `base/go.mod`、`cli/go.mod` 和各自 `go.sum`：Tink v2.8.0、klauspost/compress v1.20.0、go-sql-driver/mysql v1.9.3、go-mysql v1.13.0、TiDB parser `526b2c79173d`、goose v3.26.0、modernc SQLite v1.38.2、flock v0.12.1、cron v3.0.1、Cobra v1.9.1、Viper v1.20.1；sqlc 生成器固定 v1.30.0。已核对模块许可证：MySQL Go driver 为 MPL-2.0，go-mysql/Viper/goose/cron 为 MIT，Cobra/TiDB parser/Tink 为 Apache-2.0，flock/modernc SQLite 为 BSD 风格。本次不复制 Databasus 代码、不下载或分发 MySQL 二进制；正式发布仍需完成第 10 节第三方许可材料门禁。

### 14.2 使用方式

仓库根执行 `task build`，产物为 `bin/intern`；亦可 `go run ./cli --help`。SQL 重新生成：`task generate`。检查：`go test -race ./base/... ./cli/...`。

完整 YAML 示例在 `cli/example.yaml`。连接字段为 `connection.host/port/socket/username/password/password_env/db/tls_mode/tls_ca`；配置优先级为**显式 flag > `INTERN_` 环境变量 > YAML > 默认值**。例如 `INTERN_CONNECTION_USERNAME`、`INTERN_BACKUP_ENCRYPT`；`--encrypt=false` 可以覆盖 YAML 的 true。密码与密码引用之间也遵循来源优先级，同一 YAML 不同时设置两者。

```sh
# 先创建仅本人可访问的目录；/secure 为独立密钥位置。
mkdir -p /secure /srv/intern/backups /srv/intern/state
chmod 700 /secure /srv/intern/backups /srv/intern/state
./bin/intern keygen --key-file /secure/intern.key

# YAML 连接信息和备份目录参照 cli/example.yaml。
./bin/intern backup full --config /secure/source.yaml
./bin/intern backup incremental --config /secure/source.yaml
./bin/intern backup list --config /secure/source.yaml

# 常驻前台；每天上海时间 02:00 全量，每 15 分钟归档增量。
./bin/intern schedule run --config /secure/source.yaml \
  --full-at 02:00 --incremental-every 15m --timezone Asia/Shanghai

# 手动连接；--password 单独使用时隐藏输入。
./bin/intern backup full --host db.example.com --username backup_user \
  --password --db app --tls-ca /secure/mysql-ca.pem \
  --directory /srv/intern/backups --state-directory /srv/intern/state \
  --compress=true --encrypt=true --key-file /secure/intern.key

# target.yaml 使用相同配置结构，但 connection 指向隔离目标。
# 备份目录、state 和 key_file 仍指向待恢复材料；不需要源库在线。
./bin/intern restore --config /secure/target.yaml --backup BACKUP_ID
./bin/intern restore --config /secure/target.yaml --backup BACKUP_ID --execute
```

推荐 `password_env` 配合外部 secret 注入；如使用 YAML 明文 password 必须 `chmod 600`。直接命令行值写成 `--password=...`，可能被 shell 历史/进程参数记录。TCP 默认 `VERIFY_IDENTITY`，可给 CA；socket 默认 `DISABLED`。不允许静默降级为不可验证 TLS。

迁移/灾备需保留**备份目录 + 停止调度后完整的 state 目录 + 独立 KEK**。当前目录索引含绝对路径，异机应保持相同目录路径；未实现离线扫描重建目录索引或自动重定位。没有自动删除/保留策略，不能手工删除仍被增量引用的基线或中间项。

### 14.3 实际验证

在 Go 1.27.0、MySQL/mysqldump/mysqlbinlog 9.7.1、macOS arm64 下，以两个全新 datadir、Unix socket、`--skip-networking` 隔离实例执行真实 `intern` 命令，不连接用户现有数据库：

- 全量 + 两份增量，第二份跨显式 binlog 轮转；恢复到新库名，INSERT/UPDATE/DELETE 与源库一致。
- 加密/压缩四种组合均完成真实备份与 SQL 恢复；空流、多段流、大小上限另有回归覆盖。
- 原库名恢复视图、procedure、trigger、event，验证视图查询、procedure 返回、trigger 实际行为以及数据库 collation。
- YAML username/password/db、环境变量与 flags 优先级、显式 false 覆盖 true、终端隐藏输入；明文密码配置权限不足被拒绝。
- 调度真实跨过每日触发分钟，观察间隔增量 → 每日全量 → 增量；目录并发占用被拒绝，SIGTERM 后目录锁释放。
- 实际复现 `ALTER TABLE app.t ...` 无 `USE app` 被数据库过滤静默漏掉的缺陷；修复后在写目标前拒绝。调度器遇到该错误报警并重建全量，实际恢复新基线验证包含新增列。
- 错误 KEK、被截断制品、缺失父备份、源实例目标、非空目标、重复来源 GTID、展开超限均在写目标之前拒绝。
- 实际 purge 掉所需源日志后，增量失败且 published 列表不增加；没有错误推进保护水位。
- `task build`、`go test -race ./base/... ./cli/...` 与 `golangci-lint run ./base/... ./cli/...` 通过，golangci-lint v2.13.2 报告 `0 issues`。回归覆盖完整认证后才输出、加密模式降级拒绝、保留旧 KEK 的轮换恢复、DDL 范围过滤、routine DDL 分类、SQLite 崩溃责任与发布幂等。

### 14.4 不得扩大宣称的边界

- 实际恢复矩阵目前只有 **MySQL 9.7.1**。工具要求与服务器同主次版本；代码准入 8.4/9.x 不等于 8.4、其他补丁版本、Linux、TLS 握手、云平台或最小权限账号已实测。
- 单库恢复不是实例 PITR；不恢复用户/授权或跨库事务的整体原子语义。跨库外键被拒绝，不能可靠审计的 DDL 需要新基线；全量期间禁止并发 DDL。
- 普通 ROW 表可选择新库名；含视图、routine、event、trigger 的全量在隔离实例保持原库名。任意存储 SQL、动态 SQL 或显式 schema 引用不做猜测式改名。
- 当前按归档检查点恢复，不提供任意 datetime/事务位置输入。单个 binlog event 上限 64 MiB，不接受 transaction payload compression；增量发布与恢复均使用 `--require-row-format` 门禁。
- GTID OFF 场景没有完成实际恢复/日志重置矩阵；源日志文件编号复用、故障切换、多来源 GTID、跨版本升级等复杂拓扑仍需专项演练。不能将普通 UUID/GTID 检查夸大为全部故障切换已验证。
- Web、S3、XtraBackup 物理全量/增量、保留依赖图删除、连续断线归档和完整发布许可包均未实现。本次是用户指定的 CLI 逻辑链，不删除这些整体产品的后续验收门禁。

### 14.5 小功能提交与最终检查

- 提交规则以 `AGENTS.md` 为准：每个独立小功能/小业务单独提交，一句简短 message；不把完整 CLI 混成一次提交。
- `.golangci.yml` 已迁移到有效 v2 字段，保留原 linter 集合和复杂度阈值，取消只看 Git diff 的过滤以覆盖首次新增源码；没有通过降低阈值或 blanket suppression 取得通过。
- 为满足检查并保持可维护性，将加密读写、密钥生命周期、全量/增量、恢复计划/认证链、配置解析和调度拆为职责明确的函数与文件；统一处理文件、SQL 连接、管道和锁的关闭错误。
- `base/fsutil` 使用 Go `os.Root` 固定目录后打开文件，拒绝末级符号链接逃逸；有真实文件系统回归验证外部文件不会被截断。官方 MySQL 工具通过固定可执行文件列表调用，参数仅作为 argv，不经过 shell。
- `.githooks/pre-commit` 对暂存区业务源码分类，在独立暂存区快照运行 build、`go test -race` 和 `golangci-lint`，不依赖未暂存代码。纯文档、普通注释/格式及测试文件单独变更跳过业务检查；Go 编译指令、新增、删除和实际 AST 变化不会被当作普通注释跳过。分类器已用独立 Git fixture 验证。
- 项目命令：`task check` 运行完整业务检查，`task lint` 单独 lint，`task fmt` 整理 import。本仓库已设置 `core.hooksPath=Intern/.githooks`；新 clone 应按实际项目目录设置该相对路径。
- 重构后的已提交 CLI 再次在两个隔离 MySQL 9.7.1 实例验证：混合 codec 的全量 + 两次增量、轮转和改库名恢复；四种加密/压缩组合；YAML/环境/flags 优先级与错误密钥拒绝；每日触发分钟及间隔调度。数据行完全一致，两个实例均正常退出，临时密码、密钥和数据目录清理，不接触用户现有实例。
