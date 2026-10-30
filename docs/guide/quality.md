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
- CI 的固定汇总门禁必须核对依赖成功、预期案例集合、矩阵完整与 skip=0；required 门禁按提交 SHA 从 CircleCI 流水线记录，GitHub 不做技术强制，合并门禁按 [PR 规范](../workflow/pr.md) 的流程执行。CI 位置：CircleCI（`.circleci/config.yml`）是活跃 CI 与合并门禁；GitHub Actions 工作流 `.github/workflows/quality.yml` 已改为在 Windows 自托管 runner 上运行，使用同一矩阵和同一组 `verify-*-evidence.sh` 门禁，仍只保留手动触发（workflow_dispatch），在该 runner 上整套矩阵对同一 SHA 全绿之前不是合并门禁；runner 前置条件与切换条件见下文「CI 执行环境」。九个证据门禁（physical、ftp、replication、W3、W4、P4、W5、D1、S2）以及候选制品门禁 `verify-candidate-artifact.sh` 都要求 `GITHUB_SHA` 非空且与流内记录的源码身份一致，空的或未设置的 `GITHUB_SHA` 一律拒绝；候选制品门禁还要求 `candidate-artifact.json` 的镜像 ID、`docker save` 摘要与字节数、`intern-web` 摘要与同一次运行日志一致，固定镜像与版本与 `internal/physical/compat.json` 一致。`w4`、`p4`、`w5`、`d1`、`s2` 的 `*-gate-cases.sh` 与 `candidate-artifact-cases.sh` 在两边 quality-fast 中运行各自门禁的接受/拒绝用例（含外来 SHA 与空 SHA；d1、s2 与候选制品用例还要求每个变异真的改变了输入），门禁被悄悄放宽会在这里失败。quality-fast 同时把 `app/web/api/openapi.yaml` 连同 `info.version` 与 SHA-256 作为 `contract-openapi` 制品发布。

## CI 执行环境（Windows 自托管 runner）

`.github/workflows/quality.yml` 的全部 job 运行在仓库的 Windows 自托管 runner 上（标签 `self-hosted`、`Windows`、`X64`）；一台机器，job 串行。迁移与探测记录在 [#46](https://github.com/flyingEirc/Intern/issues/46)。下表是 2026-10-30 对 runner `JACKVANCE`（Windows 11 家庭中文版 26200、14 GB 内存、runner 2.338.0 以 NETWORK SERVICE 服务账户运行）的实测与工作流的对应处理，不是对其他机器的承诺。

| 项 | 实测 | 工作流的处理 |
| --- | --- | --- |
| 外壳 | PATH 上没有 bash；PowerShell 执行策略禁止脚本文件，`shell: powershell` 的每一步都失败 | `.github/actions/windows-runner` 用 cmd 把 Git for Windows 的 `bin` 加入 PATH，bash 步骤在 Git Bash 运行 |
| jq | 未安装，Git for Windows 不带 | 固定 jq 1.7.1，每次校验 SHA-256；PATH 上的包装脚本强制 `--binary`，否则原生 jq 输出 CRLF，门禁的逐字节比较会失败 |
| 换行 | 系统 `core.autocrlf=true` | `.gitattributes` 固定 LF；`.sh`、Dockerfile 与 OpenAPI 摘要都依赖 LF |
| Go | 未安装 | `actions/setup-go` 1.27.0；模块代理列表先 `goproxy.cn`，`go.sum` / `go.work.sum` 仍锁定每个模块 |
| Linux 执行 | 无 Docker、无 WSL；Windows 原生 `go test` 在 `app/web` 多个包失败（NTFS 没有 0700 目录、创建符号链接需特权、`syscall.SetsockoptInt` 无法编译） | build、race 测试与 lint 在 digest 固定的 Linux Go 容器中运行（`.github/scripts/linux-go.sh`，源码以 `git archive HEAD` 入卷）；真实组件 job 在主机运行 `go test`，worker 交叉编译为 linux/amd64 后在容器内运行 |
| 网络 | 到 github.com、`proxy.golang.org`、Docker Hub 的直连不稳定或被阻断，checkout 多次超时 | 属机器配置，仓库不写死代理；见前置条件 2 |

前置条件，缺任一项时对应 job 明确失败，不跳过、不回退：

1. runner 服务账户可达的原生 Linux amd64 Docker 引擎（`docker version` 报告 `linux/amd64`），Windows 上即 Docker Desktop 的 Linux 容器（WSL2 后端），且 runner 的服务账户能访问它。`.github/scripts/require-linux-docker.sh` 在每个需要容器的 job 中、第一个容器步骤之前检查，不满足即失败。
2. 机器的网络稳定到达 github.com、Docker Hub、`ghcr.io`、`repo.mysql.com`、`download.pureftpd.org` 与 Go 模块代理，使 Docker 能拉取 `internal/physical/compat.json`、`app/cli/internal/qualitygate/contract.json` 固定的镜像。代理或镜像配置在机器上（runner 目录的 `.env`、Docker 的代理设置），不进仓库。
3. Git for Windows（含 `bin\bash.exe`）。jq、Go 与 golangci-lint 由工作流按固定版本安装。
4. 内存与磁盘：CircleCI 执行器为 4 vCPU / 15 GB（`large`）；该 runner 的内存总量为 14 GB，Docker 引擎实际能分到多少、矩阵能否在其上通过，以实际运行为准。

当前状态（2026-10-30）：该 runner 没有 Docker 引擎也没有 WSL，`quality-fast` 的 Linux 步骤与全部 `container-*` job 只会在引擎检查处失败，整套矩阵尚未在其上通过。工作流因此保持手动触发，CircleCI 继续作为合并门禁。

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
