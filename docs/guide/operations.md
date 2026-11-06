# Intern Web 运维使用说明

本文面向部署和值守 Intern Web 控制面的人，只描述**仓库里真实存在的入口**：命令、flag、路径、端口、错误码都对照 `app/web/cmd/`、`app/web/deploy/`、`app/web/api/openapi.yaml` 和 `.github/workflows/quality.yml` 写出，不含设想中的功能。

产品范围、版本决策和验收边界不在这里重复，见 [Web 备份控制面与流式管道](../roadmap/Web备份控制面与流式管道-2026-10-17.md)（部署模型第 2 节、预检第 3.2 节、恢复第 5 节、测试与验收第 13 节）与 [Web 优先总路线](../roadmap/Web优先与多应用架构-2026-10-17.md)（阶段表第 3 节、证据第 5 节）。检查与验收规则见 [质量规范](quality.md)。

## 0. 状态标记

| 标记 | 含义 |
| --- | --- |
| 已实现 | 代码在仓库中，可用 `read`/`grep` 对照；不等于已验收。 |
| 已原生验收 | 该组件组合在 Linux amd64 真实容器 CI 通过：**MySQL 8.4.11 + Percona XtraBackup 8.4.0-7 + Pure-FTPd 1.0.54**（阶段 D 与阶段 E 的 D1/S2 记录，见总路线第 5 节 2026-10-29 与 2026-11-05 条目；矩阵本身仍是 `candidate`，未改为 verified）。 |
| 未验证 | 本轮没有证据，不得当作支持声明。 |

阶段 E 状态（父 Issue [#15](https://github.com/flyingEirc/Intern/issues/15)；子 Issue：[D1 #41](https://github.com/flyingEirc/Intern/issues/41) 镜像与部署验收、[S2 #42](https://github.com/flyingEirc/Intern/issues/42) FTPS、[V1 #43](https://github.com/flyingEirc/Intern/issues/43) 内网实测（已取消）、[G #44](https://github.com/flyingEirc/Intern/issues/44) 门禁与本文）：阶段 E 的候选提交 `9489f6c` 已在 `feat/web-stage-e` 上原生验收，GitHub Actions [run 41859225449](https://github.com/flyingEirc/Intern/actions/runs/41859225449)（2026-11-04，自托管 runner，14 个 job 全绿，约 48 分钟）同时覆盖 D1、S2 与 G。D1 先在 `9e05e52` 由 CircleCI 流水线 `71202872` 验收（历史记录），再在 `9489f6c` 重新确认，包含 `ed794d2` 的健康检查修复；S2 在 `container-ftps` 用真实 Pure-FTPd 1.0.54 TLS 夹具通过显式与隐式两种模式，四类拒绝（`tls_untrusted`、`tls_hostname`、`tls_expired`、`tls_negotiation`）都发生在发送凭据之前，并从仅 FTPS 副本恢复增量 1、增量 2 到全新 `mysql:8.4.11`，独立快照一致；G 的候选制品门禁与 `contract-openapi` 契约制品（`info.version` 0.3.0，附 SHA-256）也在同一次运行产出。`9489f6c` 已随 [PR #45](https://github.com/flyingEirc/Intern/pull/45) 于 2026-11-04 合入 main（merge `2a2040d`），main 上的 [run 41871999890](https://github.com/flyingEirc/Intern/actions/runs/41871999890) 14 个 job 全绿；其后的 F3（FTPS 问候语归类）、范围判定用例、D1 Compose 清理与本文档同步在 `c68dee4` 的 [run 41876148900](https://github.com/flyingEirc/Intern/actions/runs/41876148900) 验收（14 个 job 全绿），经后续 PR 合入 main。V1（[#43](https://github.com/flyingEirc/Intern/issues/43)）于 2026-11-05 由用户取消，阶段 E [#15](https://github.com/flyingEirc/Intern/issues/15) 因此关闭；截至 2026-11-05 最近一次整套 main 运行为 merge `7355766` 的 push [run 41975617492](https://github.com/flyingEirc/Intern/actions/runs/41975617492)，14/14 job 全绿（含 `container-ftp`、`container-replication`、`container-restore`、`container-restore-api`、`container-ftps`、`container-deploy` 与 `quality-gate`；自托管 runner `JackVance`，linux/amd64）。**验收结论以对应候选提交的运行证据与 Issue 记录为准**，本文不代替它。凡引用 `app/web/integration/d1_test.go` 的行为，都是上述 D1 验收执行的用例断言；引用 S2 用例的行为同样已由 `9489f6c` 的运行执行；能直接对照代码或配置文件的部分另行标注。明确**未交付**：

- V1 用户内网 FTP 实测：已于 2026-11-05 由用户取消，本项目不再获取第三方或内网服务器证据；FTP/FTPS 由每次涉及代码或配置的合并前手动运行、main 推送与每日定时运行的真实容器 CI（固定 Pure-FTPd 1.0.54 夹具）覆盖，运维须按第 12 节自测自有服务器。
- 前端（阶段 F [#19](https://github.com/flyingEirc/Intern/issues/19)）：控制台由镜像中的 `intern-web`（`webui` 构建）在 API 的同一地址提供（例如 `https://<主机>:<端口>/login`），按登录账号的权限显示：登录、会话失效处理、注销与修改本人密码（F1 #54）；仓库、数据源（含真实预检）与策略（F2 #56）；调度预览、立即全量/增量、运行列表与实时日志、取消、副本队列与重试、备份链与本地/异地保护点（F3 #57）；恢复向导（只读计划、管理员二次输入密码、演练与 `verify_sql`）、恢复详情与终态副作用（F4 #58）；用户管理与审计日志（F5 #59，仅 admin）。页面只调用本文同一组 API，服务端逐请求鉴权；本文的操作步骤以命令行与 API 书写，界面上的同名操作结果相同。
- binlog 归档与完整物理 PITR（N1）、镜像仓库公开发布、自动保留策略（N2）、K8s / Helm 与执行 agent（N3）。
- 公开镜像发布：CI 只构建私有候选制品；镜像内 `/usr/share/licenses/intern/NOTICE-WEB.txt` 与 `GPL-SOURCE-OFFER.txt` 都明确：外部分发前必须完成对应源码提供安排，本仓库未宣称已完成。

## 1. 前置条件

| 条件 | 要求 | 依据 |
| --- | --- | --- |
| 宿主机 | Linux **amd64**，运行 Docker Engine 与 **Compose v2**（`docker compose version`）。 | 镜像按 `--platform linux/amd64` 构建；`compat.json` 只有 `linux/amd64` 条目。 |
| MySQL 8.4.11 | 以 Docker 服务运行，datadir 与 socket 在命名卷上，与 Intern **同一宿主机**。 | 控制面方案第 2.1 节；`internal/physical/compat.json` |
| 版本矩阵 | `mysql:8.4.11@sha256:85b9bf2e…`、`percona/percona-xtrabackup:8.4.0-7@sha256:bf1478c1…`（`status: candidate`、`verified: false`）。 | `internal/physical/compat.json`；矩阵外版本在预检与计划阶段被拒绝 |
| 运行身份 | 默认 `999:999`（官方 mysql 镜像的 mysql 用户，也是 datadir 属主）；Compose 可覆盖 `user:`。 | `app/web/deploy/compose.yaml`、`Dockerfile` |
| 磁盘预算 | `intern-work`：按“最大一条完整链展开大小 × 恢复并发”；`intern-backups`：至少一条完整链 + 一次进行中的全量；export 宿主机目录：一条完整链展开。另设 `INTERN_MIN_WORK_BYTES` / `INTERN_MIN_BACKUP_BYTES` 下限。 | 控制面方案第 2.3、3.2 节；`flags.go` 的 `--min-work-bytes` / `--min-backup-bytes` |
| TLS 证书 | 一张对客户端使用的主机名有效（例如 `localhost` 或内网 DNS 名）的证书与私钥。 | `compose.yaml` 头部说明；`validateTLS` |
| KEK | 一个 Tink AES-128-GCM JSON keyset 文件（第 5 节）。 | `internal/artifact/key.go` |
| 备份账号 | 源实例上的 `'intern_bkp'@'localhost'`，权限模板见控制面方案第 3.2 节。 | 预检解析 `SHOW GRANTS` 并实际查询 `performance_schema.log_status` |

其他 MySQL 版本、非 amd64 架构、非 Docker 宿主机、跨主机部署**均未验证**；在这些组合上不要引用本文作为支持依据。

## 2. 部署参考栈

参考部署文件（全部在仓库内；候选验收测试 `app/web/integration/d1_test.go` 驱动的就是这些文件本身，而不是渲染出来的副本）：

| 文件 | 作用 |
| --- | --- |
| `app/web/deploy/compose.yaml` | `mysql` + `intern` 两个服务、命名卷、file secrets、健康检查、资源上限。 |
| `app/web/deploy/.env.example` | 运维必须填写的变量清单；不含任何真实凭据。 |
| `app/web/deploy/Dockerfile` | 多阶段镜像：Go 1.27.0 构建 `intern-web`，运行层以钉定 `mysql:8.4.11` 为基座叠加 PXB 8.4.0-7 的 `xtrabackup`/`xbstream`/`/usr/lib/private`/`libprocps.so.8`。 |
| `app/web/deploy/licenses/` | 镜像内 `/usr/share/licenses/intern` 的材料来源（GPL 源码提供说明等）。 |

配置优先级与其他入口一致：**显式 flags > `INTERN_*` 环境变量 > YAML 配置文件 > flag 默认值**（`app/web/cmd/flags.go`）。YAML 用 `--config` 或 `INTERN_CONFIG` 指定，键名就是下面表里的设置名（`metadata_dir`、`work_root`、`export_root`、`kek_file`、`xtrabackup_path`、`xbstream_path`、`mysqld_path`、`min_work_bytes`、`min_backup_bytes`、`restore_expansion_factor`、`restore_use_memory`、`shutdown_timeout`、`trusted_proxies`、`tls_terminated_by_proxy`、`listen`、`log.level`、`tls.cert_file`、`tls.key_file`、`session.idle_timeout`、`session.lifetime`）；环境变量名是把点与短横换成下划线再加前缀，例如 `INTERN_MIN_WORK_BYTES`、`INTERN_TLS_CERT_FILE`。配置文件必须存在且是常规文件（缺失或目录都会被拒绝，不会静默当空配置）；`restore_expansion_factor` 默认 8，`restore_use_memory` 默认 0（0 表示不给 xtrabackup 传 `--use-memory`）。

### 2.1 容器内布局与默认路径

| 容器内路径 | 来源 | 读写 | 用途 |
| --- | --- | --- | --- |
| `/var/lib/intern/metadata` | 命名卷 `intern-metadata` | 读写 | SQLite 单文件控制面 + catalog + 复制状态；`0700`，属主 `999:999` |
| `/var/lib/intern/backups` | 命名卷 `intern-backups` | 读写 | 本地主仓库（先写这里再发布） |
| `/var/lib/intern/work` | 命名卷 `intern-work` | 读写 | xtrabackup 临时目录、解包、prepare、演练 |
| `/var/lib/intern/export` | 宿主机目录 bind mount（`INTERN_EXPORT_HOST_DIR`） | 读写 | 恢复导出，供新的 mysql 容器挂载；`0750` |
| `/var/lib/mysql` | 与 mysql 服务同一个卷 | **只读 `:ro`** | xtrabackup 读取源 datadir；写入会被拒绝 |
| `/var/run/mysqld` | 与 mysql 服务同一个卷 | 读写 | `mysqld.sock`；备份账号是 `@'localhost'`，因此每次连接都是本地 socket 连接 |
| `/run/secrets/intern_kek` | Compose file secret | 只读 | KEK |
| `/run/secrets/intern_tls_cert`、`/run/secrets/intern_tls_key` | Compose file secret | 只读 | HTTPS 证书与私钥 |
| `/usr/bin/xtrabackup`、`/usr/bin/xbstream`、`/usr/sbin/mysqld` | 镜像内置 | 只读 | 备份、解包、恢复演练 |
| `/usr/share/licenses/intern/` | 镜像内置 | 只读 | `NOTICE-WEB.txt`（本镜像的第三方声明）、`VERSIONS.txt`（构建期生成的来源与逐项摘要留痕）、`GPL-SOURCE-OFFER.txt`、MySQL 与 PXB 许可证文本、`procps-ng/` 两份文本、`go-internals/`（`GO-MODULES.txt` 清单与所链接模块的许可证文本） |

默认路径以 `INTERN_*` 环境变量或配置文件给出；`app/web/deploy/compose.yaml` 显式写死了上表全部路径，也仍可覆盖。这些默认与 `intern-web` 自身的 flag 默认并不相同：`--metadata-dir` 的 flag 默认是 `/var/lib/intern/metadata`，其余（`--work-root`、`--export-root`、`--kek-file`、三个工具路径、两个下限）默认空/0，未配置时服务照常启动，只是相关操作回答 503（第 11 节）。

### 2.2 步骤

在仓库根执行。先读 `app/web/deploy/.env.example` 的头部说明，它列出了 secret 的权限要求。

```sh
cd app/web/deploy
cp .env.example .env                      # 然后编辑 .env：MYSQL_ROOT_PASSWORD、INTERN_VERSION、INTERN_SOURCE_SHA、INTERN_EXPORT_HOST_DIR 等
mkdir -p secrets exports
chown 999:999 exports && chmod 0750 exports
```

KEK 用同一仓库构建的 CLI 生成（`intern-web` 二进制与镜像都**没有** keygen 子命令，见第 5 节）：

```sh
cd ../../..                               # 从 app/web/deploy 回仓库根（keygen 与构建都在根目录）
mise run build                            # 产出 bin/intern 与 bin/intern-web
./bin/intern keygen --key-file "$PWD/app/web/deploy/secrets/intern_kek"
chown 999:999 app/web/deploy/secrets/intern_kek && chmod 0600 app/web/deploy/secrets/intern_kek
```

TLS 材料放 `secrets/intern_tls_cert`（`0644`）与 `secrets/intern_tls_key`（`0600`、属主 `999:999`）。Compose 的 file secret 就是普通 bind mount，所以宿主机文件必须是常规文件、不是符号链接，且 KEK 权限不对时 `serve` 直接拒绝启动。

构建并启动（仍在 `app/web/deploy` 下，compose 会自己把构建上下文解析到仓库根）：

```sh
cd app/web/deploy
docker compose build                      # 使用 .env 里的 INTERN_MYSQL_IMAGE / INTERN_PXB_IMAGE / INTERN_SOURCE_SHA
docker compose up -d
docker compose ps
docker compose logs -f intern
```

`docker compose build` 与候选验收测试使用的显式形式等价（在仓库根执行，标签约定 `intern:<INTERN_VERSION>-mysql8.4`）：

```sh
docker build --platform linux/amd64 --file app/web/deploy/Dockerfile \
  --tag "intern:${INTERN_VERSION:-local}-mysql8.4" \
  --build-arg SOURCE_SHA="$INTERN_SOURCE_SHA" \
  --build-arg MYSQL_IMAGE="$INTERN_MYSQL_IMAGE" \
  --build-arg PXB_IMAGE="$INTERN_PXB_IMAGE" .
```

镜像内三个工具必须能加载（构建期即校验），可现场核对与 `compat.json` 是否一致：

```sh
docker compose exec -T intern mysqld --version
docker compose exec -T intern xtrabackup --version
docker compose exec -T intern xbstream --version
docker compose exec -T intern cat /usr/share/licenses/intern/VERSIONS.txt
```

就绪与存活：

```sh
docker compose exec -T intern intern-web healthcheck   # 镜像不带 curl，二进制自己探测 /healthz
docker compose exec -T intern intern-web migrate status # JSON 迁移状态
```

`compose.yaml` 里 `intern` 服务的 healthcheck 就是 `/usr/local/bin/intern-web healthcheck`（`interval: 10s`、`timeout: 5s`、`retries: 6`、`start_period: 20s`）。`healthcheck` 用与 `serve` 相同的 `INTERN_LISTEN` 与 TLS 设置推导探测地址，绑定 `0.0.0.0` 时改用回环；**只有回环目标**才跳过证书校验（容器探测自己），任何其他目标都按系统根正常校验；探测不带凭据、响应最多读 16 KiB。可用 `--url` 指定地址、`--timeout` 设定整体时限（必须为正）。

起栈之后、创建数据源之前，还要在源实例里建备份账号：`docker compose exec mysql mysql -uroot -p`（密码从 `.env` 的 `MYSQL_ROOT_PASSWORD` 取，不要写进命令行历史），授权模板见 [控制面方案第 3.2 节](../roadmap/Web备份控制面与流式管道-2026-10-17.md)；账号必须是 `'intern_bkp'@'localhost'` 而不是按容器 IP 授权的账号，因为参考部署的每次连接都走共享 socket 卷。若策略启用 `page_tracking`，还需要 `SELECT` on `mysql.component`（预检查询 `component_mysqlbackup`）。参考部署不创建业务库：第 6 节建数据源后先用 `…/preflight` 验证，再建策略。

停止：`docker compose stop intern`（默认宽限期来自 `stop_grace_period: 120s`，见第 9 节）。**不要用 `docker compose down -v`**：`-v` 会删除命名卷，metadata、backups 与 work 随之丢失；`docker compose down`（不带 `-v`）只删容器和网络。

## 3. 首个管理员

`serve` 启动时自动应用迁移，但不会创建用户。第一个管理员用容器内二进制创建，**不需要停服务**：

```sh
# 密码来自受控文件（0600，用完即删）或你的 secret 管理器的输出，绝不放进命令行参数
docker compose exec -T intern intern-web user create-admin \
  --username ops --password-stdin < /run/secrets/ops_initial_password
```

- 密码只从 stdin（`--password-stdin`，读一行）或终端两次无回显输入读取，**永远不接受命令行参数密码**；非终端且没带 `--password-stdin` 会报 `stdin is not a terminal; use --password-stdin`。
- `--username` 规则：`^[A-Za-z0-9._-]{3,64}$`。
- 成功输出 JSON `{"id":"…"}`，用户角色固定为 `admin`，并与 `users.create_admin` 审计行同事务提交。
- 该子命令本身不校验密码长度；12–256 字符是 **API 契约**（`Password` schema）对 `POST /api/v1/users` 的要求。给管理员请用符合该范围的密码，与其他入口保持一致。
- 后续用户与角色（`admin` / `operator` / `viewer`）由管理员经 API 管理：`GET|POST /api/v1/users`（创建必须给 `role`）、`GET|PATCH /api/v1/users/{id}`（字段为 `role`、`disabled`、`password`，至少一个；会吊销该用户会话；最后一个启用的 `admin` 不能被禁用或降级）、`DELETE /api/v1/users/{id}/sessions`（只吊销会话）；权限矩阵与限速规则以 `app/web/api/openapi.yaml` 为准。自己的密码用 `PUT /api/v1/auth/password` 改（会撤销其他会话）。

登录（会话 cookie）：

```sh
API=https://127.0.0.1:8443
CA=secrets/intern_ca.pem                  # 信任锚：签发上面那张证书的 CA（示例路径，按你的 PKI 放）
JAR=/tmp/intern.jar
curl --cacert "$CA" -c "$JAR" -sS -X POST "$API/api/v1/auth/login" \
  -H 'Content-Type: application/json' -d @login.json
```

`login.json` 形如 `{"username":"ops","password":"…"}`，用后即删，不要落进 shell 历史。登录限速：每 IP 30 次/分钟（burst 30），每 IP+用户名 5 次/分钟（burst 5）；超限是 `429 rate_limited` 并带 `Retry-After`。会话 cookie 是 `intern_session`，`Secure`、`HttpOnly`、`SameSite=Lax`、`Path=/`，**没有关闭 Secure 的选项**；默认 30 分钟空闲、12 小时绝对寿命（`--session-idle-timeout` / `--session-lifetime`，空闲不得超过绝对寿命）。

示例中的 `curl` 不带 `Origin`：跨源防护（Go `CrossOriginProtection`）把无 `Origin`/无 `Sec-Fetch-Site` 的请求视为非浏览器请求放行，`GET`/`HEAD`/`OPTIONS` 总是允许；浏览器跨站请求会被拒为 `403 cross_origin_rejected` 并写拒绝审计。若你显式发送 `Origin`，它必须与 `Host` 一致（候选部署用例正是发送同源 `Origin` 调用 API）。

## 4. TLS 与反向代理

两种受支持的形态，二选一。

**最低 TLS 版本**：控制面监听器固定 `MinVersion = TLS 1.2`（`app/web/internal/server/server.go`），没有下调开关；FTP 副本的 FTPS 会话同样最低 TLS 1.2 并要求控制与数据通道都加密（第 13 节）。

**证书从哪里来**：仓库里没有面向运维的证书签发入口——`intern-web` 只有 `serve`/`migrate`/`user`/`healthcheck` 四个子命令，`app/web/deploy/` 也没有签发脚本；`app/cli/internal/testenv/tls.go` 里的 CA 与服务器证书生成带 `//go:build integration`，只服务验收 fixture，不能当部署工具用。因此证书必须由你自己的 PKI 或既有内部 CA 签发，并要求对**客户端实际使用的主机名**有效（例如 `localhost` 或内网 DNS 名）；监听器在启动时就用 `tls.LoadX509KeyPair` 校验证书与私钥成对且可解析，不匹配即拒绝启动。

**内置 TLS（参考部署采用）**

```sh
intern-web serve --tls-cert-file /run/secrets/intern_tls_cert --tls-key-file /run/secrets/intern_tls_key
```

- 两个 flag 必须成对给出，否则启动失败：`tls.cert_file and tls.key_file must be supplied together`；证书与私钥加载不了时报 `cannot load TLS certificate and private key: …`。YAML 对应 `tls.cert_file` / `tls.key_file`，环境变量对应 `INTERN_TLS_CERT_FILE` / `INTERN_TLS_KEY_FILE`。
- **非回环明文监听被直接拒绝**：`non-loopback HTTP requires TLS certificates or tls_terminated_by_proxy`。`--listen` 默认 `127.0.0.1:8080`；参考部署用 `INTERN_LISTEN=0.0.0.0:8443` + 内置证书，宿主机只把 `127.0.0.1:8443` 发布出来。

**代理终止 TLS**

```sh
intern-web serve --listen 127.0.0.1:8080 --tls-terminated-by-proxy --trusted-proxies 127.0.0.1
```

- `--tls-terminated-by-proxy` 必须同时给 `--trusted-proxies`，否则报 `tls_terminated_by_proxy requires trusted_proxies and does not prove end-to-end TLS`；只配代理列表不证明 TLS。
- 这不是端到端 TLS：必须用防火墙让客户端连不上后端端口，启动日志会固定输出这条 WARN（`serving plain HTTP behind a TLS-terminating proxy; firewall this listener …`）。
- `--trusted-proxies` 接受 IP 或 CIDR，默认 none；它决定 Gin 是否采信 `X-Forwarded-For`，直接影响**按 IP 的登录限速**归属。`validateProxy` 沿用 `gin.Engine.SetTrustedProxies` 的准入规则（带 zone 的 IPv6 会被拒绝）。
- 这种形态下浏览器到代理仍是 HTTPS，`Secure` cookie 正常工作；Intern 到代理之间是明文，按上面的隔离要求处理。

## 5. KEK

| 事项 | 事实 |
| --- | --- |
| 是什么 | Tink AES-128-GCM 的 JSON keyset，由 `internal/artifact.GenerateKey` 生成；CLI `intern keygen` 走同一个函数，候选部署验收 fixture 也是。 |
| Web 侧生成 | `intern-web` **没有** keygen 子命令，镜像里也只有 `intern-web`。实操做法：在同仓库构建 `./bin/intern keygen --key-file <path>`，再把文件放进 Compose secret 指向的宿主路径。 |
| 权限 | 必须是常规文件、非符号链接、权限去掉任何组/其他位（`chmod 600`）、不超过 64 KiB。缺失报 `cannot read key file`；`0644` 报 `key file must be a regular private file (chmod 600), not a symlink`；两者都是**启动即拒绝**，不是按请求失败。 |
| 位置 | 必须在 `metadata_dir`、`work_root`、`export_root` 之外（启动时检查）；创建或更新本地仓库时还会检查它不在该仓库 root 之内。`export_root` 不得包含 KEK。 |
| 用途 | 封存数据源密码与 FTP 仓库密码（AAD 绑定 `table:id:field`），并包装制品数据密钥。没有 KEK 时 `serve` 照常启动，涉及凭据的写操作与物理备份回答 503。 |
| 覆盖 | 不可覆盖生成（`O_EXCL`）：已有文件时报错而不是替换。 |
| 轮换 | 支持**由 Tink 管理的多键 keyset**：在同一 keyset 文件里保留旧键、把新键设为主键；新写入用主键，旧密文仍按内嵌 key id 用旧键解密，因此**轮换后旧备份与已封存的凭据依旧可读**。不自制轮换格式，没有独立的轮换命令，也没有 KMS 集成（决策见[详细方案第 14.1 节](../roadmap/数据库备份与恢复-2026-10-17.md)）。删除旧键等于让引用它的备份与凭据不可解密。**Web 侧凭据封存的轮换未单独验收（未验证）**；删除整个 KEK 会让所有已封存秘密与已加密链失效。 |
| 灾备 | KEK 与卷分开备份。metadata/backups/work 卷 + KEK 三者齐全才可能继续增量与恢复。 |

## 6. 备份操作（API）

以下示例针对第 2 节的参考栈：`$API=https://127.0.0.1:8443`、`--cacert $CA`、`-b/-c $JAR`。ID 从响应里复制（示例用 `jq -r .id`，jq 只是便利工具，不是依赖）。接口字段、状态码与权限以 `app/web/api/openapi.yaml` 为唯一契约。

```sh
# 1) 本地主仓库（kind=local 时 tls_mode 固定 "none"，root 必须是绝对路径）
REPO=$(curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/repositories" \
  -H 'Content-Type: application/json' \
  -d '{"name":"local","kind":"local","tls_mode":"none","root":"/var/lib/intern/backups"}' | jq -r .id)

# 2) FTP 副本仓库（可选；创建必须给 password，需要 KEK）
#    tls_mode ∈ plain|explicit|implicit，创建后不可变；plain 的响应 plaintext_credentials=true
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/repositories" \
  -H 'Content-Type: application/json' \
  -d '{"name":"opsftp","kind":"ftp","tls_mode":"plain",
       "ftp":{"host":"ftp.internal","port":21,"base_path":"/intern","username":"intern_bkp"},
       "password":"READ_FROM_SECRET_STORE"}'

# 2b) FTPS 副本仓库：explicit（AUTH TLS，常见端口 21）或 implicit（首字节即 TLS，常见端口 990）
#     ca_pem 是信任的 CA 证书 PEM（≤ 256 KiB，只能含 CERTIFICATE 块，不合规是 422）；省略时用系统信任库。
#     最低 TLS 1.2，主机名按 ftp.host 校验，控制与数据通道都加密；服务器不支持 TLS、
#     证书不受信任/主机名不符/过期时在发送 USER/PASS 之前失败，不会回退到明文。
#     tls_mode 创建后不可变（改动是 422），plaintext_credentials=false
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/repositories" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n --rawfile ca ops-ftp-ca.pem '{name:"opsftps",kind:"ftp",tls_mode:"explicit",
       ftp:{host:"ftp.internal",port:21,base_path:"/intern",username:"intern_bkp",ca_pem:$ca},
       password:"READ_FROM_SECRET_STORE"}')"

# 3) 数据源：走共享 socket，账号是 'intern_bkp'@'localhost'
SRC=$(curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/sources" \
  -H 'Content-Type: application/json' \
  -d '{"name":"mysql","socket":"/var/run/mysqld/mysqld.sock","datadir":"/var/lib/mysql",
       "username":"intern_bkp","password":"READ_FROM_SECRET_STORE"}' | jq -r .id)

# 4) 真实预检（60 秒上限；失败也是 200，body 里 ok=false + 结构化 error）
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/sources/$SRC/preflight" \
  -H 'Content-Type: application/json' -d "{\"local_repository_id\":\"$REPO\"}"

# 5) 仓库可写探测（仅 admin；FTP 按其 tls_mode 打开并带本实例命名空间标记，不发布对象）
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/repositories/$REPO/probe"

# 6) 策略：字段全部必填，replica_repository_id 可为 null
POLICY=$(curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/policies" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"core\",\"source_id\":\"$SRC\",\"local_repository_id\":\"$REPO\",
       \"replica_repository_id\":null,\"full_cron\":\"0 2 * * *\",\"incremental_cron\":\"0 * * * *\",
       \"timezone\":\"UTC\",\"max_chain_length\":5,\"parallel\":1,\"throttle\":0,
       \"zstd_level\":3,\"page_tracking\":false,\"enabled\":false}" | jq -r .id)

# 7) 启用（服务端先跑真实预检与副本探测，全部成功才在同一事务里置 enabled=1）
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/policies/$POLICY/enable"

# 8) 手动触发一次全量（Idempotency-Key 必须，1–128 字符）
RUN=$(curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/policies/$POLICY/runs" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: ops-2026-10-29-full-1' \
  -d '{"kind":"full"}' | jq -r .run_id)
```

`full_cron` / `incremental_cron` 是 5 字段标准 cron（`cron.ParseStandard`），`timezone` 必须是非空 IANA 名称（空串与 `Local` 被拒）。`throttle` 直接映射为 `xtrabackup --throttle=N`：`0` 表示不下传该参数（不限速），`>0` 表示每秒最多读 N 个块，**值越小越慢**（候选验收用例用 `throttle: 1` 拖住一次真实全量来覆盖 SIGTERM 窗口）。`parallel ≥ 1`（低于 1 按 1 传给 xtrabackup）、`max_chain_length ≥ 1`、`zstd_level` 1–22（编码端并发固定为 1）、`page_tracking` 给 xtrabackup 加 `--page-tracking`，依赖 `component_mysqlbackup`。创建或更新策略时直接把 `enabled` 置 `true` 与调用 `…/enable` 走同一条路径：先跑真实预检与副本探测，不过就整条请求拒绝（`409 preflight_failed`，预检输入不全则是 `503`），不落任何行。停用：`POST /api/v1/policies/{id}/disable`（不需要预检）。调度器在 `serve` 进程内常驻，容器停了调度就停。

## 7. 状态查看

| 想知道什么 | 命令 |
| --- | --- |
| 进程存活（不碰数据库） | `GET /healthz`（无需会话） |
| 迁移与数据库就绪 | `GET /readyz`（未就绪 503）；`intern-web migrate status` |
| 运行列表 / 单条 | `GET /api/v1/runs?policy_id=&source_id=&status=&limit=&before=`；`GET /api/v1/runs/{id}` |
| 运行事件（分页） | `GET /api/v1/runs/{id}/logs?after_seq=&limit=`（每行上限 1000，超出时末行是截断标记且 `truncated=true`） |
| 运行事件（实时） | `curl --cacert "$CA" -b "$JAR" -N -H 'Accept: text/event-stream' "$API/api/v1/runs/$RUN/events"` |
| 链与制品 | `GET /api/v1/sources/{id}/chains`（成员带 kind、seq、status、presence、LSN、载荷身份）；`GET /api/v1/backups/{id}`（含每个仓库的副本行） |
| 复制队列 | `GET /api/v1/replicas?source_id=&repository_id=&status=&limit=`（status ∈ pending, uploading, uploaded, verified, failed, exhausted） |
| 本地/异地保护点（RPO） | `GET /api/v1/sources/{id}/protection`（两侧只有 **verified** 副本才推进，`uploaded` 不算） |
| 恢复列表 / 单条 / 事件 | `GET /api/v1/restores?backup_id=&status=&limit=&before=`；`GET /api/v1/restores/{id}`；`GET /api/v1/restores/{id}/logs?after_seq=&limit=` |
| 审计 | `GET /api/v1/audit-logs?limit=&before=`（仅 admin，`audit:read`） |

状态迁移：运行 `queued → running → completed | failed | canceled`（没有 finalizing 态，物理链提交是链运行的内部步骤）。重试一条复制：`POST /api/v1/backups/{id}/replicas/{repository_id}/retry`（`replicas:retry`，admin 或 operator），把该行重置为 pending 并立即踢一次 worker。

SSE 要点：`runs:read` 校验在第一个字节之前完成（失败是普通 JSON）；开始后只关流不混入 JSON。事件 `id` 是 `run_logs.seq`，每 15 秒 `: ping` 心跳并在心跳处复查会话与权限，失效时发一个 `revoked` 事件后关闭；`Last-Event-ID` 头优先于 `after_seq` 查询参数做重放；每订阅者缓冲 256 个事件，溢出只丢订阅者、不影响运行。**`replication_enqueued` 与 `replication_step` 在终止事件之后才追加**，因此实时流拿不到它们——需要复制结论时，运行终止后读 `runs/{id}/logs` 或轮询 `replicas` API。恢复没有 SSE，轮询 `restores/{id}` 与 `restores/{id}/logs`。

## 8. 恢复与导出使用

```sh
# 1) 只读计划：不落恢复行、不写文件、不碰 FTP
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/restores/plan" \
  -H 'Content-Type: application/json' -d "{\"backup_id\":\"$BACKUP\"}"
# → links（每一环的来源 local|ftp）、target_to_lsn、required/available work 与 export 字节、
#   export_path（含 {restore_id} 占位）、risks、rehearsal_available、plan_digest

# 2) 显式执行：只有 admin（restores:execute），且必须重新输入管理员密码
curl --cacert "$CA" -b "$JAR" -sS -X POST "$API/api/v1/restores" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: ops-restore-1' \
  -d "{\"backup_id\":\"$BACKUP\",\"plan_digest\":\"<64位十六进制>\",
       \"password\":\"ADMIN_PASSWORD_REENTER\",\"rehearse\":true,\"verify_sql\":[]}"
```

- `plan_digest` 必须等于执行时重新计算出的新计划，否则 `409 plan_changed`；密码错是 `403 confirmation_failed`，写拒绝审计且不落恢复行；二次确认与改密共用同一每用户限速预算（5 次/分钟），所以一个会话总共 5 次机会，不是各 5 次。
- `rehearse=true` 而部署没有 `mysqld_path` 是 `422 validation_failed`。`verify_sql` 由恢复引擎自己的边界校验：最多 16 条、每条 1–4096 字节、无 NUL、无内部语句分隔符（允许一个结尾 `;`）、必须以 `SELECT` 开头；在只读事务里执行，且不写审计。
- 计划拒绝的机器码在 `error.detail.code`：`404 backup_not_found`、`422 matrix_unsupported`、其余拒绝一律 `409`（如 `chain_broken`、`lsn_discontinuous`、`source_unavailable`、`tool_version_mismatch`、`key_unavailable`、`insufficient_space`），恢复运行时未配置是 `503 unconfigured`。
- 备份与恢复共享一个执行通道，全局互斥 `running_max=1`：另一条队列在跑时不会被认领。

导出使用：完成的恢复把已 prepare 的 datadir 以**原子、不覆盖的 rename** 发布到 `<export_root>/<restore_id>/data`，`GET /api/v1/restores/{id}` 的 `export_path` 就是实际路径；映射到宿主机即 `${INTERN_EXPORT_HOST_DIR}/<restore_id>/data`（目录 `0750`，属主是容器 UID）。**已发布的导出永不被取消或重启回收删除**，由运维自行清理。

用一个新的 mysql 容器启动这份导出（阶段 D 已原生验收的启动形式，参数与 `app/web/integration/p4_test.go` / `d1_test.go` 采用的一致）：

```sh
docker run --rm --platform linux/amd64 -u 999:999 \
  -v "$PWD/exports:/exports" \
  "$INTERN_MYSQL_IMAGE" \
  --datadir=/exports/<RESTORE_ID>/data --socket=/tmp/mysql.sock --pid-file=/tmp/mysqld.pid \
  --port=3306 --skip-log-bin --innodb-buffer-pool-size=64M
```

也可以按控制面方案第 5.2 节给出的挂载形态 `docker run -v <宿主机导出路径>/<restore_id>/data:/var/lib/mysql "$INTERN_MYSQL_IMAGE"` 启动（镜像 UID 同为 999）。替换生产实例前必须：先停源库、核对 `auto.cnf` 的 `server-uuid`（同一 `server_uuid` 不能与源实例同时接入复制拓扑）、按本地配置调整。Intern **从不自动替换生产 datadir**。

取消：`POST /api/v1/runs/{id}/cancel`（`runs:cancel`）与 `POST /api/v1/restores/{id}/cancel`（`restores:execute`，仅 admin）。排队中的任务直接变 canceled；运行中的任务取消上下文，但**成功点优先**——物理链已完成的运行仍是 completed，导出已 rename 到位的恢复仍是 completed；取消永不删除备份，也永不删除已发布导出；对已终止的任务取消是 `409`。

## 9. SIGTERM、宽限期与中断语义

`intern-web serve` 收到 SIGTERM/SIGINT 后按顺序：停止接收新请求 → 在 `--shutdown-timeout`（默认 30s）窗口内优雅关停 → 排干 dispatcher 的两个 worker。注意 flag 自己的说明：**这只是关闭窗口，不是进程退出的绝对死线**；窗口之后在途 handler 仍可能继续排干（分离审计预算 + SQLite busy 超时 + 少量余量，当前约 10.5s）。取消一个请求不会打断 SQLite 的 busy 等待或 fsync。

因此 `compose.yaml` 声明 `stop_grace_period: 120s`（候选用例要求 ≥ 90s），运行中的备份/恢复才有机会走上面的取消语义而不是被 SIGKILL。候选用例的做法是保持一个被 `throttle=1` 拖住的真实全量在飞行中，再用 `docker compose stop -t <grace+30> intern`，断言停止在声明的宽限期内完成、容器退出码为 0、没有 OOMKilled。

被中断的运行**永不记成 completed**，`backup_id` 为空，completed 计数不增长：在宽限期内完成的优雅 SIGTERM 会把仍在复制的运行记为 `failed`、`error_class=backup_failed`；进程被强杀（SIGKILL/崩溃）时，下一次启动的恢复流程把遗留的 `running` 行收敛为 `failed`、`error_class=interrupted`；只有显式取消才是 `canceled`。若 worker 活过了 bounded 排干窗口，`serve` 会返回错误（不吞掉），状态交给下次启动回收。已发布导出与已提交备份不受影响。

## 10. 升级、重建与回滚

业务数据（仓库、数据源、策略、备份、恢复记录、用户）只存在 `intern-metadata` 卷的 SQLite 中，载荷只在 `intern-backups`，KEK 只在 file secret 里，**升级镜像不覆盖任何这些内容**。

```sh
# 升级：只改标签指向的构建，再让同一个服务换新镜像
# .env: INTERN_VERSION=<新版本或新提交>
docker compose build
docker compose up -d intern
docker compose exec -T intern intern-web migrate status   # 三个 *_pending 都为 0、current==latest 即 Ready
```

回滚是对称操作：把 `INTERN_VERSION` 改回旧标签（旧镜像仍在宿主机上时 `docker compose build` 可跳过），再 `docker compose up -d intern`。**回滚的证据边界**：升级方向已由 D1 验收覆盖（同 Dockerfile 构建第二个不同 `SOURCE_SHA` 标签、替换后 `migrate status` 为 Ready、用户/资源/备份/恢复记录与 KEK 均保留、可继续增量且 parent 指向上一次头；候选 `9e05e52`，`9489f6c` 的运行再次通过），但升级后没有再做一次载荷恢复；**降级到更早的迁移版本没有任何证据**——若新版本已经前推 metadata 迁移，回滚旧镜像可能读到更高的 `current_version`，回滚前请先备份 `intern-metadata` 卷。

`docker compose restart intern` 与 `docker compose up --force-recreate intern` 保留卷与 KEK 已由 D1 验收断言（候选 `9e05e52`，`9489f6c` 再次通过）；数据只存在卷与 file secret 里、镜像层不含业务数据，这一点也可由 `compose.yaml` 的卷声明直接对照。同一 `intern-metadata` 卷上启动第二个容器会被独占锁拒绝：`metadata directory is already in use by another intern process`（`app/web/internal/store/files.go`），且不影响第一个容器（单实例 SQLite、单调度所有者）。

制品留痕（CI 的做法，运维需要自备候选制品时同样适用）：

```sh
docker image inspect --format '{{.Id}}' "intern:${INTERN_VERSION:-local}-mysql8.4"
docker save "intern:${INTERN_VERSION:-local}-mysql8.4" | sha256sum
sha256sum bin/intern-web
```

上述留痕正是门禁 G 要绑定的字段。`.github/workflows/quality.yml` 的 `container-deploy` job 跑 `TestD1NativeContainers`，导出 `INTERN_D1_ARTIFACT_DIR`，产出 `.cache/quality/deploy/candidate-artifact.json` 与 `events.jsonl`，再用 `actions/upload-artifact` 上传为 `deploy-d1-${{ github.sha }}` 制品；`.github/scripts/verify-d1-evidence.sh` 把镜像 ID / 标签 / `docker save` 摘要 / `intern-web` 摘要 / 源 SHA 与 `GITHUB_SHA` 绑定，`.github/scripts/verify-candidate-artifact.sh` 再打开 `candidate-artifact.json`，要求其中的身份与同一次运行日志一致、固定镜像与版本与 `internal/physical/compat.json` 一致；`d1-gate-cases.sh` 与 `candidate-artifact-cases.sh` 守住两个门禁不被悄悄放宽。quality-fast 把 `app/web/api/openapi.yaml`（`info.version` 0.3.0）连同 SHA-256 作为 `contract-openapi` 制品发布。D1 先在候选 `9e05e52` 验收；`9489f6c` 的 [run 41859225449](https://github.com/flyingEirc/Intern/actions/runs/41859225449) 同时通过 D1、候选制品门禁与契约发布（[#44](https://github.com/flyingEirc/Intern/issues/44)），该次运行的 `candidate-artifact.json` 记录镜像 ID `sha256:781526c4…`，`contract-openapi` 记录 `info.version=0.3.0`、`sha256=a30145b3…489b`。这些改动已随 [PR #45](https://github.com/flyingEirc/Intern/pull/45) 合入 main（merge `2a2040d`，main 上 [run 41871999890](https://github.com/flyingEirc/Intern/actions/runs/41871999890) 14 个 job 全绿）；之后的 F3、范围判定用例、D1 Compose 清理与文档同步在 `c68dee4` 的 [run 41876148900](https://github.com/flyingEirc/Intern/actions/runs/41876148900) 验收，经后续 PR 合入 main。**CI 只构建私有候选，没有公开发布镜像**；镜像内的 `/usr/share/licenses/intern/`（`NOTICE`、`VERSIONS.txt`、`GPL-SOURCE-OFFER.txt`、MySQL 与 PXB 许可证文本）是 GPL 材料，不等于源码提供安排已完成。

## 11. 故障处理

先分层：`/healthz` 只看进程，`/readyz` 看数据库连通与迁移版本，业务失败先看 HTTP 状态码与 `error.code`（`app/web/api/openapi.yaml` 的 `ErrorResponse`），再看 `error.detail.code`。响应都带 `X-Request-ID`，把它和脱敏后的请求一起写进 Issue（[Issue 流程](../workflow/issue.md)）。

| 症状 | 状态码 / 固定文案 | 原因与处置 |
| --- | --- | --- |
| `serve` 起不来 | `cannot read key file` | KEK 文件不存在或不可读。检查 Compose file secret 的宿主路径与 `INTERN_KEK_FILE`。 |
| `serve` 起不来 | `key file must be a regular private file (chmod 600), not a symlink` | KEK 权限带组/其他位或是符号链接。`chmod 0600` 并 `chown 999:999`。 |
| `serve` 起不来 | `metadata directory is already in use by another intern process` | 同一 `intern-metadata` 卷已有容器（常见于 `docker compose run`）。一个卷只允许一个容器。 |
| `serve` 起不来 | `non-loopback HTTP requires TLS certificates or tls_terminated_by_proxy` | 非回环监听既没有证书也没有声明代理终止。给 `--tls-cert-file`/`--tls-key-file`，或 `--tls-terminated-by-proxy` + `--trusted-proxies` 并防火墙隔离。 |
| `serve` 起不来 | `tls.cert_file and tls.key_file must be supplied together` / `cannot load TLS certificate and private key: …` | 只给一个，或证书与私钥不匹配/不可解析。 |
| `serve` 起不来 | `tls_terminated_by_proxy requires trusted_proxies and does not prove end-to-end TLS` | 声明了代理终止但没给可信代理列表。 |
| `serve` 起不来 | `export_root must be outside metadata_dir` / `… must be outside export_root` / `export_root must not contain kek_file` / `export_root must be a non-symlink directory` | 导出目录与受保护目录重叠或是符号链接/文件。按第 2.1 节分卷。 |
| `serve` 起不来 | `metadata_dir must be a non-empty absolute path`；`<setting> must be an absolute path` | 相对路径。所有路径设置必须是绝对路径。 |
| `serve` 起不来 | `min_work_bytes must be positive when min_backup_bytes is set`（及反向） | 两个空间下限必须同时为正或同时为 0。 |
| 备份相关操作 | `503 unavailable`（触发手动运行、启用策略、预检） | 预检输入不全：KEK、`work_root`、`xtrabackup_path` 或两个 `min_*_bytes` 缺失/为 0（`Settings.Preflight()`）。部署仍可启动，只让这些操作不可用。 |
| 恢复计划/执行 | `503`，`error.detail.code = "unconfigured"` | 恢复输入缺一个：KEK、`work_root`、`xtrabackup_path`、`xbstream_path`、`export_root`（`Settings.Restore()`）。 |
| `rehearse:true` | `422 validation_failed` | 没配 `mysqld_path`。没有 mysqld 仍可恢复，只是不能演练。 |
| 启用策略 | `409 preflight_failed`，结构化结果在 `error.preflight` | 真实预检或副本探测没过；`enabled` 保持 0。按 `error.preflight` 里的检查名处置（`privileges`/`privileges_missing`、`identity`（挂错卷，`auto.cnf` 与 `@@server_uuid` 不符）、`version`/`tool_version_mismatch`、`socket`、空间不足等）。 |
| 写凭据、探测仓库 | `503 unavailable` | 没有 KEK 就无法封存/打开秘密。 |
| 浏览器/客户端 | `403 cross_origin_rejected` | 跨源防护拒绝（`Origin` 与 `Host` 不一致，或 `Sec-Fetch-Site` 非 same-origin/none）。用一致的主机名访问，或让代理保持同源。 |
| 登录、改密、二次确认 | `429 rate_limited` + `Retry-After` | 命中限速：登录 30/分钟/IP 与 5/分钟/IP+用户名；改密与恢复确认各 5/分钟/用户（同一预算）。 |
| 需要 argon2 的请求 | `503 unavailable` + `Retry-After` | 每进程最多 2 个并发 argon2id，匿名登录校验只占其中 1 个槽；占满时立即返回 503，**不排队**。降低并发或按 `Retry-After` 稍后重试。 |
| 恢复计划 | `409 plan_changed` | 计划已过期（链或空间状况变了）。重新 `POST /api/v1/restores/plan` 并用新的 `plan_digest`。 |
| 恢复执行 | `403 confirmation_failed` | 二次确认密码不对；写拒绝审计，无恢复行。 |
| 容器不健康 | `health: starting` → `unhealthy` | `/healthz` 没在 `start_period: 20s` + `retries: 6` 内答 200。看 `docker compose logs intern`；`intern-web healthcheck` 单独跑一次能拿到失败原因（只打印 scheme+host，不带路径/查询）。 |
| 停止卡住或被杀 | 退出码 137 | `stop_grace_period` 小于实际关闭窗口，或运行中的任务无法在窗口内收尾。保持默认 `120s`，先 `POST …/cancel` 再停。 |
| 任何 `/api/v1` 请求 | `401 unauthorized` | 没有会话、会话过期（默认 30 分钟空闲 / 12 小时绝对）、改密或管理员吊销后失效。重新 `POST /api/v1/auth/login` 并复用同一个 cookie jar；`GET /api/v1/auth/session` 可确认当前身份与有效权限。 |
| 某个具体操作 | `403 forbidden` | 角色权限不足（三角色与每项权限见 `app/web/api/openapi.yaml`），或该路由本就限 admin（仓库探测、用户管理、恢复执行与取消）。写拒绝审计；不是重试信号，换角色或换人。 |

## 12. 自有 FTP/FTPS 服务器自测（原 V1）

原 V1（[#43](https://github.com/flyingEirc/Intern/issues/43)）计划对用户的运维 FTP 服务器做内网实测，2026-11-05 由用户取消：本项目**没有**、也不再获取第三方或内网 FTP 服务器上的实测证据。仓库里可复现的 FTP/FTPS 证据只有每次涉及代码或配置的合并前手动运行、main 推送与每日定时运行的真实容器 CI 使用的固定夹具：`container-ftp`（Pure-FTPd 1.0.54，19 个明文场景）、`container-replication`（全量 + 两次增量的复制与回读）、`container-restore` / `container-restore-api`（从仅 FTP 副本恢复增量 1、2）、`container-ftps`（显式/隐式 FTPS、四类凭据前拒绝）。服务器软件与版本、被动模式/NAT、带宽与耗时都随环境而异，**运维必须用本节清单自测自己的服务器**，结果留在自己的记录里；未自测前不得把它当作已验证。

自测时按下表记录字段，全部脱敏（不上传密码、KEK、私钥、完整 SQL dump/raw binlog，也不上传含 secret 的 `docker inspect`；参照[质量规范](quality.md)的证据要求）：

| 组 | 要记录的字段 |
| --- | --- |
| 服务器 | FTP 服务器软件与版本（例如 `Pure-FTPd` 的实际版本号，来自服务器自身公告）、传输模式（`plain` / `explicit` / `implicit`）、端口、是否被动模式与端口范围、TLS 证书签发方与有效期（FTPS 时）、服务器侧留存策略 |
| 拓扑与版本 | 被测源码 SHA、`intern:<INTERN_VERSION>-mysql8.4` 的镜像 ID、`docker save` SHA-256、`mysqld`/`xtrabackup`/`xbstream` 版本输出、宿主机与 Docker/Compose 版本 |
| 凭据边界 | 使用的 FTP 账号名与可写 `base_path`（**不记录密码、不记录可定位的主机名/IP**，用占位符标注）、`has_secret`、`plaintext_credentials` 的实际值 |
| 带宽 | 测量方法与数值：宿主机到服务器的上行/下行 Mbps（同一窗口内采样），以及同窗口内一次已知大小传输的用时；注明是否与其他流量共享 |
| 耗时 | 全量、增量 1、增量 2 各自的：run 时长、载荷字节、`payload_sha256`；每次复制到 FTP 的排队→`uploaded`→`verified` 三段耗时与重试次数；从 FTP 副本恢复的：计划用时、解包、prepare、导出发布、总时长 |
| 结果 | 每步的 `run_id` / `backup_id` / `restore_id`、`status`、`error_class`、`error_code`、`GET /api/v1/sources/{id}/protection` 的 local 与 remote 两个点、副本 `status` 序列 |
| 一致性 | FTP 副本来源恢复后由新 mysql 容器启动读取的独立快照与期望快照的比对结果（至少完成一次仅从 FTP/FTPS 副本的恢复） |
| 脱敏输出 | 命令、API 响应、`docker compose ps` 与日志片段的白名单版本（去掉秘密与可定位地址） |
| 结论 | 成功/失败/未执行，分别对应明文与 FTPS 路径；失败时给出最小复现步骤 |

最小闭环：明文 FTP 完成**一次全量 + 两次增量**的复制与回读认证，并**从副本完成一次恢复**（删掉本地载荷，只从副本恢复；只在测试栈或数据副本上执行，且先确认该备份的副本 `status` 为 `verified`——恢复计划只对 verified 副本回退到 FTP，见 `internal/restore/plan.go`）；同一服务器再重复 FTPS（显式与隐式）路径。任一项没有在本服务器上的真实结果时，只能标注为**未验证**。

## 13. 限制与支持矩阵

- **Web 首版未交付**：阶段 F（[#19](https://github.com/flyingEirc/Intern/issues/19)）的页面已实现，但涉及真实组件的界面端到端验收（F2–F4、界面全链路 #65）目前只有 arm64 上模拟 linux/amd64 的开发证据，原生 Linux amd64 验收与 CI 结果未完成；本文不构成首版交付声明。
- **支持矩阵**：`internal/physical/compat.json` 只有一条记录 —— `mysql-community` 8.4.11 + PXB 8.4.0-7 + `linux/amd64`，`status: "candidate"`、`verified: false`，证据 Issue 是 [#27](https://github.com/flyingEirc/Intern/issues/27)。矩阵外版本、其他 MySQL 版本、arm64、云平台、TLS/最小权限的其他组合都**未验证**，未验证前不得列入已支持范围（[质量规范](quality.md)）。阶段 D 已用上述组件在真实容器中完成恢复与 prepare（已原生验收），阶段 E 的部署形态由 `container-deploy` 在候选 `9e05e52`（CircleCI 历史记录）与 `9489f6c`（[run 41859225449](https://github.com/flyingEirc/Intern/actions/runs/41859225449)）验收；矩阵本身未改。
- **PITR**：首版只有物理全量 + 页增量链的**检查点恢复**；binlog 归档与完整物理 PITR 属 N1，未交付，不得宣称。
- **仓库后端**：只有本地仓库 + FTP/FTPS 副本；S3/SFTP 未实现。自动保留清理属 N2（未交付），当前保留是手动判定，未 verified 的副本不会被删除。
- **单所有者**：单实例 SQLite + 单调度所有者；多个 Web 用户不等于多副本调度或高可用；MySQL 与 Intern 不同宿主机、K8s/Helm、跨主机 agent 属 N3，未实现。
- **FTPS（S2）**：`internal/storage/ftp/tls.go` 与 `tls_mode`（`explicit`/`implicit`）、`ca_pem` 契约已实现并在候选 `9489f6c` 验收：最低 TLS 1.2、控制与数据通道都加密（数据通道在传输命令发出后握手）、拒绝降级、校验不受信任 CA/主机名/过期，四类拒绝（`tls_untrusted`、`tls_hostname`、`tls_expired`、`tls_negotiation`）都在发送凭据之前。`tls_negotiation` 只表示 TLS 握手失败或服务器拒绝 AUTH TLS；问候语里的拒绝（如 `421 Too many users`，显式模式在 AUTH 之前、隐式模式在握手之后）按普通 FTP 回复归类为 `reply` 并保留 `reply_code`（`c68dee4` 的 run 41876148900）。`container-ftps` 用真实 Pure-FTPd 1.0.54 夹具跑显式与隐式，并从仅 FTPS 副本恢复增量 1、2（run 41859225449）。**内网 FTPS（V1-A2）随 V1 于 2026-11-05 取消，未验证**，不能用夹具结果外推。
- **明文 FTP**：凭据明文过网，响应里 `plaintext_credentials: true` 常驻标注；只在受控内网使用，且服务器行为须由运维按第 12 节自测。
- **非夹具 FTP/FTPS 服务器**：FTP/FTPS 证据仅来自固定 Pure-FTPd 1.0.54 夹具；其他服务器软件/版本、被动模式/NAT 后端、带宽与耗时**没有证据**，V1 内网实测已于 2026-11-05 取消，不得把夹具结果外推为任意服务器已验证。
- **磁盘与明文工作文件**：备份与恢复会在受控目录产生受保护的明文工作文件（`work`、`.pending-*` 导出、解包目录），需要独立磁盘权限治理；恢复认证暂存由清理账本负责，不承诺完全无明文落盘。
- **镜像许可**：镜像携带 MySQL 与 PXB 二进制属于 GPLv2 分发行为，另含 PXB 私有库目录与 `libprocps.so.8`（LGPL）以及 `intern-web` 静态链接的 Go 模块；CI 只构建私有候选、未公开发布，外部分发前必须完成对应源码提供安排。逐项文本与来源见镜像内 `/usr/share/licenses/intern/NOTICE-WEB.txt`、`VERSIONS.txt` 与 `go-internals/GO-MODULES.txt`（`app/web/deploy/licenses/`）。

## 14. 入口

| 需要 | 去哪里 |
| --- | --- |
| 部署与存储决策 | [控制面方案第 2 节](../roadmap/Web备份控制面与流式管道-2026-10-17.md)、[详细方案第 6.2.1 节](../roadmap/数据库备份与恢复-2026-10-17.md) |
| 预检项与授权模板 | 控制面方案第 3.2 节 |
| 恢复/导出/演练流程与拒绝边界 | 控制面方案第 5 节、第 13 节 |
| 接口契约（唯一） | [`app/web/api/openapi.yaml`](../../app/web/api/openapi.yaml) |
| 部署文件 | [`app/web/deploy/compose.yaml`](../../app/web/deploy/compose.yaml)、[`.env.example`](../../app/web/deploy/.env.example)、[`Dockerfile`](../../app/web/deploy/Dockerfile) |
| 命令行入口 | [`app/web/cmd/flags.go`](../../app/web/cmd/flags.go)、[`root.go`](../../app/web/cmd/root.go)、[`user.go`](../../app/web/cmd/user.go)、[`healthcheck.go`](../../app/web/cmd/healthcheck.go) |
| 部署验收用例 | [`app/web/integration/d1_test.go`](../../app/web/integration/d1_test.go)、[`.github/workflows/quality.yml`](../../.github/workflows/quality.yml) 的 `container-deploy` |
| 版本矩阵 | [`internal/physical/compat.json`](../../internal/physical/compat.json) |
| 阶段与证据 | [总路线第 3、5 节](../roadmap/Web优先与多应用架构-2026-10-17.md)；门禁 [#44](https://github.com/flyingEirc/Intern/issues/44) |
| 开发与检查命令 | [开发规范](development.md)、[质量规范](quality.md) |
