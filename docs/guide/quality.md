# 质量与验收规范

具体产品场景与支持矩阵以 [产品路线第 12、14、15 节](../roadmap/数据库备份与恢复-2026-10-17.md) 为准；验收记录写入对应 [Issue](../workflow/issue.md)，在 [PR](../workflow/pr.md) 逐项汇总。

## 按改动范围选择检查

| 变更类型 | 本地提交前检查 | CI / 合并 / 发布要求 |
| --- | --- | --- |
| 业务逻辑 | build、test、配置中的 golangci-lint 全部通过 | 相关真实组件场景；发布还核对候选制品和声明矩阵。 |
| 测试、fixture、依赖、镜像、工具链或 workflow | 运行与改动相符的验证；本地 hook 分类不等于验收完成 | 必须纳入 CI 影响范围，不能因不是业务 Go 源码而漏测。 |
| 纯文档、普通注释、提交规范 | 不运行 build/test/lint；检查内容、链接、差异与暂存范围 | 可明确重型测试不需要执行，不得描述成容器测试已通过。 |

业务本地检查在 Intern 根执行 `mise run check`，对应：

```sh
go build ./internal/... ./app/cli/... ./app/web/...
go test -race ./internal/... ./app/cli/... ./app/web/...
golangci-lint run ./internal/... ./app/cli/... ./app/web/...
```

lint 必须使用项目 `.golangci.yml` 与 `golangci-lint`，不得以 `go vet` 替代，不能降规则、调低门槛或跳过失败检查。本地 `.githooks/pre-commit` 对暂存快照分类并执行检查；它的豁免范围不替代 CI 要求。

## 真实组件是核心业务门禁

- 涉及外部组件的核心业务，以真实组件与实际候选程序/容器的集成结果作为合并、发布门禁；mock 只补充纯逻辑边界，不能代替真实备份恢复验收。
- 编排使用已定 Go testing + Testcontainers 方案。消费者可观察的数据、对象行为与恢复点是判断依据，不能只看容器启动、端口、文件非空或退出码为零。
- 分离源/目标实例、身份与数据；恢复全量和至少两个增量，包含轮转，验证选中恢复点的数据，而不是后来变化的当前源。独立查询预期值与对象行为。
- 失败场景先到达可观察注入点，再核对拒绝、目标前后状态、公开备份列表、水位和资源清理。setup 失败不能冒充成功触发业务拒绝。
- 必需入口的 runtime 不可用、镜像/工具不匹配、零用例、缺矩阵结果、skip 或取消必须阻断门禁，不得自动回退 mock 或把未执行写成通过。
- 候选 CLI/发布镜像与被测对象必须一致；源码 SHA、程序 SHA256、镜像 digest、平台和工具版本都要可追踪。测试后重建的新制品不能继承旧制品通过结论。
- CI 的固定汇总门禁必须核对依赖成功、预期案例集合、矩阵完整与 skip=0；required 门禁按提交 SHA 从 CircleCI 流水线记录，GitHub 不做技术强制，合并门禁按 [PR 规范](../workflow/pr.md) 的流程执行。CI 位置：CircleCI（`.circleci/config.yml`）是活跃 CI，与 GitHub Actions 工作流使用同一矩阵和同一组 `verify-*-evidence.sh` 门禁；`.github/workflows/quality.yml` 在仓库的 Linux 自托管 runner（Windows 主机上的 WSL2）上运行，仍只保留手动触发（workflow_dispatch），在该 runner 上整套矩阵对同一 SHA 全绿之前不是合并门禁；runner 前置条件与切换条件见下文「CI 执行环境」。九个证据门禁（physical、ftp、replication、W3、W4、P4、W5、D1、S2）以及候选制品门禁 `verify-candidate-artifact.sh` 都要求 `GITHUB_SHA` 非空且与流内记录的源码身份一致，空的或未设置的 `GITHUB_SHA` 一律拒绝；候选制品门禁还要求 `candidate-artifact.json` 的镜像 ID、`docker save` 摘要与字节数、`intern-web` 摘要与同一次运行日志一致，固定镜像与版本与 `internal/physical/compat.json` 一致。`w4`、`p4`、`w5`、`d1`、`s2` 的 `*-gate-cases.sh` 与 `candidate-artifact-cases.sh` 在两边 quality-fast 中运行各自门禁的接受/拒绝用例（含外来 SHA 与空 SHA；d1、s2 与候选制品用例还要求每个变异真的改变了输入），门禁被悄悄放宽会在这里失败。quality-fast 同时把 `app/web/api/openapi.yaml` 连同 `info.version` 与 SHA-256 作为 `contract-openapi` 制品发布。

## CI 执行环境（WSL2 里的 Linux 自托管 runner）

`.github/workflows/quality.yml` 的全部 job 运行在仓库的自托管 runner 上，标签 `self-hosted`、`Linux`、`X64`：Windows 11 主机上的 WSL2 Ubuntu 24.04，runner 与 Docker Engine 都装在这个发行版里。一个 runner 进程同一时刻只跑一个 job，只有一个 runner 时整套矩阵串行（第一次整套运行 52 分钟）；并行要在同一发行版里多装 runner 实例，受内存限制（见下文第 4 条）。选它的原因：服务只在 Linux 上运行，真实组件验收要求原生 Linux amd64 Docker（见上文）；原 CI（CircleCI 的 `machine: ubuntu-2404:current`，Actions 工作流原先的 `ubuntu-24.04`）都是 Linux 主机，`go test` 直接在宿主机上跑，harness 按这个宿主机写（路径、uid、绑定挂载目录的 chown / chmod）。WSL2 里的 Ubuntu 与它同形，工作流只需要换 `runs-on`。迁移与探测记录在 [#46](https://github.com/flyingEirc/Intern/issues/46)。

没有选 Windows runner。仓库曾在它上面做过适配（#46，分支 `ci/windows-self-hosted-runner`，未合并），代价高于收益：Docker Desktop 的引擎归登录用户的会话，runner 服务账户（NETWORK SERVICE）实测打不开它的管道（[Docker 文档](https://docs.docker.com/desktop/setup/install/windows-permission-requirements/) 同样说明，`docker-users` 组无效），要用就得让 runner 以登录用户运行并保持登录；读代码得出宿主侧 harness 不能作为 Windows 进程运行（`filepath` 拼 Linux 容器路径、`os.Getuid()`、NTFS 上的 chown / chmod 语义，未在 Windows 上运行验证），只能把所有 Go 步骤放进容器；runner 目录里 node24 的 `node.exe` 还被杀毒软件删除过一次。仓库里同时登记着 Windows runner `JACKVANCE`（标签 `Windows`），工作流不再使用它，保留或在 Settings 里移除都可以。

前置条件，缺任一项时对应 job 在第一个步骤由 `.github/scripts/require-runner.sh` 明确失败并列出全部缺项，不跳过、不回退：

1. Ubuntu 24.04（WSL2），`/etc/wsl.conf` 里 `[boot]` 下 `systemd=true`；runner 以 systemd 服务运行（`sudo ./svc.sh install <账户>`），账户不是 root。runner 目录与 `_work` 放在发行版自己的文件系统里（如 `~/actions-runner`），不放 `/mnt/c`、`/mnt/d`：那是 NTFS 经 9p 挂载，不保证 Linux 的属主与权限位语义，而 harness 与 D1 依赖绑定挂载目录上的 chown / chmod。
2. Docker Engine（Docker 官方 apt 源的 `docker-ce`、`docker-compose-plugin`、`docker-buildx-plugin`）直接装在该发行版里，账户在 `docker` 组，`docker version` 报告 `linux/amd64`。Docker Desktop 的 WSL 集成对该发行版必须关闭，否则 `docker` 指向 Desktop 的引擎，又回到对登录会话的依赖。D1（`--compose`）还要求内核能给容器的 cgroup 设 IO 权重（`io.bfq.weight` 或 `io.weight`）：仓库的 `compose.yaml` 给 `intern` 服务设了 `blkio_config.weight: 300`，D1 还断言它不能被删。WSL2 默认内核的 BFQ 是内核模块且默认没有加载（`/proc/config.gz`：`CONFIG_IOSCHED_BFQ=m`、`# CONFIG_BLK_CGROUP_IOCOST is not set`，`bfq.ko` 在 `/lib/modules/<内核版本>/kernel/block/`），没有它 `compose up` 在 runc 写 `io.weight` 时失败；需要 `sudo modprobe bfq`，并把 `bfq` 写进 `/etc/modules-load.d/bfq.conf` 让它随启动加载。`require-runner.sh --compose` 在 D1 开头用 `docker run --blkio-weight=300` 探测，缺少时在几秒内明确失败。
3. `git`、`jq`、`curl`、`build-essential`（`go test -race` 要 cgo）；Go 由 `actions/setup-go` 按 `1.27.0` 装进 runner 的工具缓存，golangci-lint 由 quality-fast 用 `go install` 装。
4. 资源与并行：CircleCI 执行器为 4 vCPU / 15 GB（`large`），每个 job 独占一台。WSL2 默认最多用主机内存的一半，这台机器（14 GB）约 7 GB；`%UserProfile%\.wslconfig` 的 `[wsl2]` 下 `memory=` 按实测调整。并行就是在同一发行版里再装 runner 实例（各自目录与名字，标签相同）。读测试代码，harness 的资源名随机或带时间戳、D1 的 `INTERN_HOST_PORT=0`，没有看到会撞的固定主机端口或名字（静态阅读，没有并发跑过）；主要限制是内存，各 job 的峰值内存没有测过，并发数以实测为准。
5. 常驻：WSL2 实例在没有进程时会被回收，runner 随之离线，需要在 Windows 侧保活（例如开机计划任务运行 `wsl.exe -d Ubuntu-24.04 --exec sleep infinity`）；Windows 本身不能睡眠或休眠（电源计划设为从不睡眠），否则 runner 同样离线（2026-11-03 Windows 上的 runner 曾连续离线一个多小时，原因没有查明）；重启后能否自动恢复要实测。
6. 网络：WSL2 默认是 NAT，到 github.com、`proxy.golang.org`、Docker Hub、`ghcr.io`、`repo.mysql.com`、`download.pureftpd.org`、`downloads.percona.com` 的通路要在 WSL 里单独确认。代理与镜像配置在机器上（runner 目录的 `.env`、Docker 守护进程配置、`.wslconfig` 的网络选项），不进仓库。

与旧的 hosted 写法的差异只有：`runs-on`；每个 job 开头的 `require-runner.sh`；`setup-go` 的 `cache: false`（runner 自己保留模块与构建缓存）；超时在原值上加 15 分钟（scope 20、quality-fast 45：第一次运行要冷拉全部固定镜像与模块）。

当前状态（2026-11-04）：runner `JackVance`（标签 `self-hosted`、`X64`、`Linux`、`docker`、`wsl`）在线：WSL2 内核 6.18.54.1、16 CPU、6859 MiB 内存、Docker Engine 29.9.3（Compose v5.7.0、buildx v0.38.1）、jq 1.7、git 2.43.0，runner 在 `/opt/actions-runner`。第一次整套运行（[41825448242](https://github.com/flyingEirc/Intern/actions/runs/41825448242)，`4107c0f`，一个 runner、job 串行，共 52 分钟）：11 个 job 通过（scope、S2、mysql 两个版本、W4、R1、W5、W3、S1、P4、P2），3 个失败：`quality-fast` 在预检处失败（主机没装 C 编译器）；`container-deploy` 的 `a1-image` 通过、`a2-compose` 因内核没有 `io.bfq.weight` 失败；`quality-gate` 因上游失败按设计失败。两个环境问题处理后要重跑，整套矩阵在该 SHA 上尚未全绿。CircleCI 一侧：自 2026-10-29 02:13 UTC 起它的流水线不再执行任何 job——此后的 47 条流水线（含 main 的每日定时流水线），`quality-fast` 的 `started_at` 都是零值，唯一步骤是 `Error: Task information unavailable`（CircleCI API）；原因没有从 API 读到，需在 CircleCI 控制台确认。最后一次真正执行的矩阵是 `9d3541b`（2026-10-29 02:03 UTC：11 个 job 通过，`container-ftps` 失败），其后的 S2 修复提交此前没有任何真实的容器矩阵结果，上面这次运行里 `container-ftps` 通过，是它们的第一份。文档规定的合并门禁仍是 CircleCI（[PR 规范](../workflow/pr.md)），本次迁移不改这条规定，工作流保持手动触发。

切换到 GitHub Actions 仅在同一 SHA 上整套矩阵（含 `quality-gate`）于该 runner 全绿之后执行：把 `quality.yml` 的触发改为 `push` / `pull_request`；把 [PR 规范](../workflow/pr.md) 的合并门禁改为该 Actions 流水线；删除 `.circleci/config.yml` 并同步各文档对 CircleCI 的现行描述（历史证据保留原日期与来源）。

## 证据与支持范围

| 状态 | 能证明什么 |
| --- | --- |
| 设计完成 | 方案、依赖和验收条件已决策，不能证明实现或运行。 |
| 代码完成 | 有对应提交和检查结果，不能自动证明真实外部组件通过。 |
| 原生进程验证 | 指定 OS/版本/工具与场景下真实执行，不能替代容器 CI。 |
| 真实容器 CI 通过 | 指定 SHA/制品/平台/矩阵下实际通过，不能外推到所有生产组合。 |

记录成功、失败、阻塞与未执行；证据至少包含对应提交/制品、环境、案例与恢复点、断言、清理结果和实际 CI URL（适用时）。诊断只保存白名单脱敏内容，不上传密码、KEK、私钥、完整 SQL dump/raw binlog 或含 secret 的容器 inspect。

新数据库、版本、平台或部署组合未验证前不得列入已支持矩阵。历史 PRD 中的实验记录保留其原日期、环境和限制；当前工作区存在实现文件、mise 任务或 workflow 不等于已经验收。CLI 容器门禁 #1–#4（承接 IotProject#5–#10）与 Web 阶段 B–E（#12–#15）的进展，以各 Issue 的实际提交与运行证据更新。

本规范定义门禁要求；每次实际执行了哪些检查、测试与验收，以对应 Issue 和 CI 证据为准。
