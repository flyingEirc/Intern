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
- CI 的固定汇总门禁必须核对依赖成功、预期案例集合、矩阵完整与 skip=0；required 门禁按提交 SHA 从 GitHub Actions 运行记录，GitHub 不做技术强制，合并门禁按 [PR 规范](../workflow/pr.md) 的流程执行。CI 位置：GitHub Actions（`.github/workflows/quality.yml`）是活跃 CI 与合并门禁，全部 job 运行在仓库的 Linux 自托管 runner（Windows 主机上的 WSL2）上；触发为所有分支与 tag 的 `push`、每日 `23 3 * * *` 定时运行（默认分支）和手动 `workflow_dispatch`，不监听 `pull_request`（只有一个 runner、job 串行，同仓 PR 分支的 `push` 已经产生整轮运行，再监听会让每次运行重复一遍）。重型 job 能否跳过由 `scope` job 判定，基点随 ref 而定：main 的 `push` 按这次推送自己的增量（`github.event.before` 到该提交），因为 main 上每个提交都有不被取消的独立运行；其他分支的 `push` 按整条分支相对 main 的累计差异（`git merge-base origin/main <head>` 到 head），不看这次推送的增量；tag、定时与手动运行没有基点，总是整套。判定范围内只改 `*.md`（`internal/`、`app/`、`.github/`、`.githooks/` 下的 Markdown 仍按代码路径处理）才跳过，`quality-gate` 仍须成功；列文件时关闭重命名检测，代码文件改名成 `*.md` 仍算代码改动。基点缺失或无法解析（`before` 为空或全零、强推后 `before` 已不在克隆里、没有 `origin/main`、`merge-base` 或 `diff` 失败）一律整套，原因写在 `scope` 日志的 `quality-scope:` 行，不让 `scope` 失败。其他分支按累计差异而不是推送增量，是因为同一分支被取代的运行会被取消：按增量判定时，纯文档的后续推送会取消（或接在失败之后）此前代码推送的整套运行，却得到成功的 `quality-gate`，让一个从未跑完整套矩阵的 head 看上去通过。这些规则由 `quality-scope-cases.sh` 在 quality-fast 中逐条自测（26 个用例，含上述后续推送、强推、缺 `origin/main`、改名、基底不可用时的 `git diff` 失败与按 actions/checkout 命令重放的克隆）。CircleCI 没有这个跳过。切换于 2026-11-04 在同一 SHA（`fa9da2e`）整套矩阵全绿（[run 41836973008](https://github.com/flyingEirc/Intern/actions/runs/41836973008)）后执行；CircleCI 自 2026-10-29 起没有可用额度，已停用并删除 `.circleci/config.yml`，历史 CircleCI 证据保留原日期与来源。runner 前置条件见下文「CI 执行环境」。九个证据门禁（physical、ftp、replication、W3、W4、P4、W5、D1、S2）以及候选制品门禁 `verify-candidate-artifact.sh` 都要求 `GITHUB_SHA` 非空且与流内记录的源码身份一致，空的或未设置的 `GITHUB_SHA` 一律拒绝；候选制品门禁还要求 `candidate-artifact.json` 的镜像 ID、`docker save` 摘要与字节数、`intern-web` 摘要与同一次运行日志一致，固定镜像与版本与 `internal/physical/compat.json` 一致。`w4`、`p4`、`w5`、`d1`、`s2` 的 `*-gate-cases.sh` 与 `candidate-artifact-cases.sh` 在 quality-fast 中运行各自门禁的接受/拒绝用例（含外来 SHA 与空 SHA；d1、s2 与候选制品用例还要求每个变异真的改变了输入），门禁被悄悄放宽会在这里失败。quality-fast 同时把 `app/web/api/openapi.yaml` 连同 `info.version` 与 SHA-256 作为 `contract-openapi` 制品发布。
- `container-mysql` 按锁定矩阵每个配置跑一次，job 内顺序是：清理脚本自测 `cli-quality-clean-cases.sh` 与停止脚本自测 `cli-quality-stop-test-cases.sh` → pre-clean 上一 job 的遗留 → 14 个真实场景 → `if: always()` 停止被取消 job 遗留的测试进程组 → `quality-gate verify --ci` 校验证据 → `if: always()` 清理本次资源 → `--inventory` 盘点 → 上传白名单证据。测试步骤用 `setsid --wait` 把 `go test | tee` 放进独立会话与进程组，组长把 pgid 写入 `RUNNER_TEMP` 下的记录文件；停止步骤只对该组先发 SIGTERM、5 秒宽限后 SIGKILL，正常完成的测试组已退出时为空操作。harness 给每个场景的容器、网络与命名卷打 `intern.quality.runner=<sha256(RUNNER_NAME)[:8]>` 与 case 标签（testcontainers v0.44.0 不会给命名卷加标签，故显式创建），清理脚本只按该 runner 标签筛选；取消的 job 不会执行 `t.Cleanup`，遗留由清理步骤移除。runner 预检的 `docker version`/`docker info` 各限 30 秒、`--compose` 的 `docker run` 探测限 120 秒，超时以 `Docker daemon not responding within N s` 失败并列出缺项，不会耗尽 job 超时。候选镜像无法带 runner 标签，按 `intern-quality:<profile>-<sha12>-<arch>` 名称范围只保留最新 3 个并始终保留当前 SHA。证据以 `container-<profile>` 制品上传 `candidate.json`、`events.jsonl`、`receipts/*.json` 三条白名单路径，`overwrite: true` 使同一 job 的重跑替换同名制品。
- 每个配置的 `quality-gate verify --ci` 与汇总 `aggregate`（内部同样按 `RequireClean` 校验）把证据绑定到本次运行：`ci_url` 必须等于 `GITHUB_SERVER_URL/GITHUB_REPOSITORY/actions/runs/GITHUB_RUN_ID`（变量缺失直接失败），候选源码不得有未提交改动，客户端工具版本、数据库与构建镜像必须等于 `contract.json` 的锁定值，每个源/目标实例的版本与镜像 ID 必须等于配置的固定 digest；`checkpoint_chain` 要求 3 个顺序检查点（全量 + 两次增量）。实例镜像 ID 与仓库 digest 的比较依赖 runner 的 containerd 镜像存储，经典 Docker 镜像存储下会失败关闭，不会放行。`TestContractMatrixIsPinned` 固定 14 个用例与 2 个配置的名称和顺序，删改、改名或调序会先在这里失败。

## CI 执行环境（活跃 CI：WSL2 里的 Linux 自托管 runner）

`.github/workflows/quality.yml` 的全部 job 运行在仓库的自托管 runner 上，标签 `self-hosted`、`Linux`、`X64`：Windows 11 主机上的 WSL2 Ubuntu 24.04，runner 与 Docker Engine 都装在这个发行版里。一个 runner 进程同一时刻只跑一个 job，只有一个 runner 时整套矩阵串行（两次整套运行都是 52 分钟）；并行要在同一发行版里多装 runner 实例，受内存限制（见下文第 4 条）。选它的原因：服务只在 Linux 上运行，真实组件验收要求原生 Linux amd64 Docker（见上文）；此前的 CI（CircleCI 的 `machine: ubuntu-2404:current`、Actions 工作流原先的 `ubuntu-24.04`）都是 Linux 主机，`go test` 直接在宿主机上跑，harness 按这个宿主机写（路径、uid、绑定挂载目录的 chown / chmod）。WSL2 里的 Ubuntu 与它同形，工作流只需要换 `runs-on`。迁移与探测记录在 [#46](https://github.com/flyingEirc/Intern/issues/46)。

没有选 Windows runner。仓库曾在它上面做过适配（#46，分支 `ci/windows-self-hosted-runner`，未合并），代价高于收益：Docker Desktop 的引擎归登录用户的会话，runner 服务账户（NETWORK SERVICE）实测打不开它的管道（[Docker 文档](https://docs.docker.com/desktop/setup/install/windows-permission-requirements/) 同样说明，`docker-users` 组无效），要用就得让 runner 以登录用户运行并保持登录；读代码得出宿主侧 harness 不能作为 Windows 进程运行（`filepath` 拼 Linux 容器路径、`os.Getuid()`、NTFS 上的 chown / chmod 语义，未在 Windows 上运行验证），只能把所有 Go 步骤放进容器；runner 目录里 node24 的 `node.exe` 还被杀毒软件删除过一次。仓库里同时登记着 Windows runner `JACKVANCE`（标签 `Windows`），工作流不再使用它，保留或在 Settings 里移除都可以。

前置条件，缺任一项时对应 job 在第一个步骤由 `.github/scripts/require-runner.sh` 明确失败并列出全部缺项，不跳过、不回退：

1. Ubuntu 24.04（WSL2），`/etc/wsl.conf` 里 `[boot]` 下 `systemd=true`；runner 以 systemd 服务运行（`sudo ./svc.sh install <账户>`），账户不是 root。runner 目录与 `_work` 放在发行版自己的文件系统里（如 `~/actions-runner`），不放 `/mnt/c`、`/mnt/d`：那是 NTFS 经 9p 挂载，不保证 Linux 的属主与权限位语义，而 harness 与 D1 依赖绑定挂载目录上的 chown / chmod。
2. Docker Engine（Docker 官方 apt 源的 `docker-ce`、`docker-compose-plugin`、`docker-buildx-plugin`）直接装在该发行版里，账户在 `docker` 组，`docker version` 报告 `linux/amd64`。Docker Desktop 的 WSL 集成对该发行版必须关闭，否则 `docker` 指向 Desktop 的引擎，又回到对登录会话的依赖。D1（`--compose`）还要求内核能给容器的 cgroup 设 IO 权重（`io.bfq.weight` 或 `io.weight`）：仓库的 `compose.yaml` 给 `intern` 服务设了 `blkio_config.weight: 300`，D1 还断言它不能被删。WSL2 默认内核的 BFQ 是内核模块且默认没有加载（`/proc/config.gz`：`CONFIG_IOSCHED_BFQ=m`、`# CONFIG_BLK_CGROUP_IOCOST is not set`，`bfq.ko` 在 `/lib/modules/<内核版本>/kernel/block/`），没有它 `compose up` 在 runc 写 `io.weight` 时失败；需要 `sudo modprobe bfq`，并把 `bfq` 写进 `/etc/modules-load.d/bfq.conf` 让它随启动加载。`require-runner.sh --compose` 在 D1 开头用 `docker run --blkio-weight=300` 探测，缺少时在几秒内明确失败。
3. `git`、`jq`、`curl`、`build-essential`（`go test -race` 要 cgo）；Go 由 `actions/setup-go` 按 `1.27.0` 装进 runner 的工具缓存，golangci-lint 由 quality-fast 用 `go install` 装。
4. 资源与并行：此前的 CircleCI 执行器为 4 vCPU / 15 GB（`large`），每个 job 独占一台。WSL2 默认最多用主机内存的一半，这台机器（14 GB）约 7 GB；`%UserProfile%\.wslconfig` 的 `[wsl2]` 下 `memory=` 按实测调整。并行就是在同一发行版里再装 runner 实例（各自目录与名字，标签相同）。读测试代码，harness 的资源名随机或带时间戳、D1 的 `INTERN_HOST_PORT=0`，没有看到会撞的固定主机端口或名字（静态阅读，没有并发跑过）；主要限制是内存，各 job 的峰值内存没有测过，并发数以实测为准。
5. 常驻：WSL2 实例在没有进程时会被回收，runner 随之离线，需要在 Windows 侧保活（例如开机计划任务运行 `wsl.exe -d Ubuntu-24.04 --exec sleep infinity`）；Windows 本身不能睡眠或休眠（电源计划设为从不睡眠），否则 runner 同样离线（2026-11-03 Windows 上的 runner 曾连续离线一个多小时，原因没有查明）；重启后能否自动恢复要实测。
6. 网络：WSL2 默认是 NAT，到 github.com、`proxy.golang.org`、Docker Hub、`ghcr.io`、`repo.mysql.com`、`download.pureftpd.org`、`downloads.percona.com` 的通路要在 WSL 里单独确认。代理与镜像配置在机器上（runner 目录的 `.env`、Docker 守护进程配置、`.wslconfig` 的网络选项），不进仓库。

与旧的 hosted 写法的差异只有：`runs-on`；每个 job 开头的 `require-runner.sh`；`setup-go` 的 `cache: false`（runner 自己保留模块与构建缓存）；超时在原值上加 15 分钟（scope 20、quality-fast 45：第一次运行要冷拉全部固定镜像与模块）。

当前状态（2026-11-04）：runner `JackVance`（标签 `self-hosted`、`X64`、`Linux`、`docker`、`wsl`）在线：WSL2 内核 6.18.54.1、16 CPU、6859 MiB 内存、Docker Engine 29.9.3（Compose v5.7.0、buildx v0.38.1）、jq 1.7、git 2.43.0，runner 在 `/opt/actions-runner`，一个 runner、job 串行。第一次整套运行（[41825448242](https://github.com/flyingEirc/Intern/actions/runs/41825448242)，`4107c0f`，52 分钟）：11 个 job 通过，3 个失败：`quality-fast` 在预检处失败（主机没装 C 编译器）；`container-deploy` 的 `a1-image` 通过、`a2-compose` 因内核没有 `io.bfq.weight` 失败；`quality-gate` 因上游失败按设计失败。装 `build-essential`、加载 `bfq` 内核模块后，第二次整套运行（[41836973008](https://github.com/flyingEirc/Intern/actions/runs/41836973008)，`fa9da2e`，52 分钟）14 个 job 全部通过：`quality-fast`（build 31 秒、race 测试 3 分 33 秒且 23 个包通过、六个门禁自测与候选制品用例、OpenAPI 契约发布、golangci-lint 0 issues）、全部容器验收（含 D1 的 a1–a5 五个用例与候选制品门禁、S2）和 `quality-gate`（`aggregate` 核对两个 mysql 矩阵项均为 `passed`、`linux/amd64`、各 14 个用例、源码 SHA `fa9da2e`）；各容器验收的 skip 与 fail 均为 0，所有步骤都是 success。这是同一 SHA 上的一次全绿，没有重复运行，所以没有稳定性数据；重启后 runner 与 `bfq` 是否自动恢复、并行下的内存与抖动都没有验证。CircleCI 一侧：自 2026-10-29 02:13 UTC 起它的流水线不再执行任何 job——此后的 47 条流水线（含 main 的每日定时流水线），`quality-fast` 的 `started_at` 都是零值，唯一步骤是 `Error: Task information unavailable`；CircleCI v2 job API 的 `messages` 给出原因 `free-plan-no-credits-available`（免费计划没有可用额度），2026-11-04 重跑工作流 `29b32528`，新的 12 个 job 仍以同一原因全部被拒。最后一次真正执行的矩阵是 `9d3541b`（2026-10-29 02:03 UTC：11 个 job 通过，`container-ftps` 失败），其后的 S2 修复提交此前没有任何真实的容器矩阵结果，上面两次运行里 `container-ftps` 都通过，是它们的第一份。文档规定的合并门禁已在该条件满足后切换为 GitHub Actions（[PR 规范](../workflow/pr.md)），切换记录见下段。

第三次整套运行（[41859225449](https://github.com/flyingEirc/Intern/actions/runs/41859225449)，`9489f6c`，2026-11-04，push 触发，约 48 分钟）14 个 job 再次全部通过，含 `container-deploy`（D1 a1–a5 与候选制品门禁）、`container-ftps`（S2）与 `quality-gate`；阶段 E 候选的 D1、S2、G 验收以这次运行为准。

PR #45 已于 2026-11-04T16:53:09Z 合入 main（merge `2a2040d`，head `9489f6c`），main 上的 push 运行 [41871999890](https://github.com/flyingEirc/Intern/actions/runs/41871999890) 14 个 job 全绿。其后的分支提交 `e1532ab`（F3 FTPS 问候语归类）、`1ff3143`（范围判定 `git diff` 失败用例）、`064c850`（D1 Compose 清理）与 `c68dee4`（文档）由 [run 41876148900](https://github.com/flyingEirc/Intern/actions/runs/41876148900)（`c68dee4`，14 个 job 全绿）验收，经后续 PR 合入 main。

`container-deploy` 现在在 D1 测试前后各做一次清理：先跑 `.github/scripts/d1-compose-clean-cases.sh`（假 docker，输出 `11 cases, 0 failed`，不需要 Docker 引擎），再由 `.github/scripts/d1-compose-clean.sh` 删除本 runner 的 D1 Compose 项目的容器、网络与卷——项目名是 `d1` + `sha256(RUNNER_NAME)` 的前 8 位十六进制 + 十六进制后缀；测试后再跑一次同样的清理（`if: always()`，两次清理各 5 分钟上限）。原因是取消的运行不会执行 `t.Cleanup`：runner 在 Linux 上只把信号发给步骤的 shell，`go test` 可能成为孤儿，到 job 结束才被 SIGKILL；`compose.yaml` 的两个服务是 `restart: unless-stopped`，testcontainers reaper 不拥有 Compose 资源，而旧的 CircleCI 一次性机器会随 VM 丢弃这些资源，这台 runner 会留下它们。边界：本次改动之前用旧命名 `d1` + 16 位十六进制留下的资源不会被匹配，需要在这台 runner 上手工检查一次（`docker ps -a --format '{{.Label "com.docker.compose.project"}} {{.Names}} {{.Status}}' | grep -E '^d1[0-9a-f]{16} '`）；主机/WSL 崩溃不会执行任何步骤，由下一次 `container-deploy` 开头的清理兜底；uid 999 的 `/tmp/TestD1NativeContainers*` 临时目录和逐提交的 `intern:<sha>` 镜像不在清理范围。整套改动不在 `9489f6c` 的运行里；它在 `c68dee4` 的 [run 41876148900](https://github.com/flyingEirc/Intern/actions/runs/41876148900) 中随 `container-deploy` 在真实 runner 上执行（14 个 job 全绿），用例本身仍是假 docker 自测；上面旧名遗留资源的手工检查不因此自动完成。

切换已于 2026-11-04 执行：`quality.yml` 的触发改为所有分支与 tag 的 `push`、每日 `23 3 * * *` 定时（默认分支）与 `workflow_dispatch`；不监听 `pull_request`——只有一个 runner、job 串行，同仓 PR 分支的 push 已经产生 52 分钟的整轮运行，再监听会让同一提交重复跑一遍。`scope` 与 `quality-gate` 跳过 `github.event.deleted` 的删除事件：Actions 会为删除分支/tag 触发一次 push 运行（删除分支时附着到默认分支），CircleCI 不在删除时触发流水线，这个跳过避免删除启动矩阵或产生门禁结果。`concurrency` 在 main 上按提交分组且不取消（每个合并提交都要有自己的绿色运行，排队中的运行也不会被后来的提交顶替），其他 ref 取消被取代的运行。`scope` 的纯文档判定相应按 ref 取基点：main 用这次推送的增量，其他分支用相对 main 合并基点的累计差异，tag、定时与手动总是整套，基点缺失或无法解析也整套且不让 `scope` 失败（规则全文见上文「CI 位置」）；最初的实现对所有推送都只看推送增量，验收发现它在其他分支上会让接在被取消或失败的代码推送之后的纯文档推送得到成功的 `quality-gate`，已改为现在的规则。合并门禁改为该工作流的 `quality-gate`（[PR 规范](../workflow/pr.md)）；删除 `.circleci/config.yml`；各文档的现行描述同步到 Actions，历史 CircleCI 证据保留原日期与来源。runner 前置条件与尚未验证的项（重复运行的稳定性、重启后 runner 与 `bfq` 自动恢复、并行内存与抖动）维持上文所述。

2026-11-05 CLI 容器门禁收尾（`fix/cli-gate-hardening`）：`main` `2a2040d`（[41871999890](https://github.com/flyingEirc/Intern/actions/runs/41871999890)）、`feat/web-stage-e` `81a8714`（[41888070274](https://github.com/flyingEirc/Intern/actions/runs/41888070274)）与 `fix/cli-gate-hardening` `c5ebc93` 第 2 次尝试（[41907540485](https://github.com/flyingEirc/Intern/actions/runs/41907540485)）均 14/14 job 全绿；加固分支的两个容器腿是 mysql-9.7.1（job 126854786585）与 mysql-8.4.11（job 126854786514），汇总 `quality-gate` 为 job 126882928914。每个配置执行 14 个用例、89 条合约断言、24 个检查点，创建并移除 183 个资源（显式创建带标签的命名卷前为 104）；`checkpoint_chain` 记录 `binlog.000003:6987 → binlog.000004:4 → binlog.000006:4`。候选 CLI 由同一源码构建，`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false ./app/cli` 的 SHA-256 为 `152b129ef0de49d48d4b8ccd54bfaaee39ccf9dfadbff489a9778768715ca8c0`。

取消路径：第 1 次尝试在候选镜像构建阶段被取消，没有场景可清理（空结果）；第 3 次尝试在场景步骤约 150 秒后被取消，随后执行的清理移除 5 个容器、2 个网络与 4 个带标签的命名卷，`--inventory` 确认本 runner 无残留，第 4 次尝试的 pre-clean 没有发现遗留。第 4 次尝试的 `quality-gate` 失败，原因是同名 `container-*` 制品存在三份，`download-artifact` 取到被取消 attempt 的最高 ID 制品；`overwrite: true` 已修复，live 证明待下一次运行。纯文档路径由探针分支的 [41901344226](https://github.com/flyingEirc/Intern/actions/runs/41901344226) 实测：`scope` 判定 `only documentation changed`、`required=false`，11 个重型 job 全部 skipped，`quality-gate` 输出 `not-required (documentation-only)`；该分支随后删除，没有后续运行。上述证据只覆盖两个锁定配置与 linux/amd64；未覆盖场景列在[路线第 15.8 节](../roadmap/数据库备份与恢复-2026-10-17.md)。

CircleCI 的 GitHub 集成仍未移除：该仓库在 CircleCI 里的项目还挂着指向 circleci.com 的 webhook，所以每次推送仍会收到一条失败的 `CircleCI Pipeline` 提交状态（`No configuration was found`）；它不是门禁、不参与上述判定，可以忽略，移除它（在 CircleCI 停用该项目构建，或删除该 webhook）属于仓库所有者操作，尚未执行。

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
