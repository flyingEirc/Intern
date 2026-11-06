// #65 (stage F, #19 acceptance 3): the whole business flow through the UI
// only, on the real Compose stack: repositories, source and preflight, policy
// enabled with an FTP replica, full and incremental backups triggered from the
// policy page, both FTP replicas verified, then the incremental restored with a
// rehearsal from the restore wizard and compared with an independent snapshot
// on a brand-new mysql:8.4.11. The API only reads back and snapshots; nothing
// is created or triggered through it.
//
//   bun e2e/journey.ts [--evidence DIR] [--image TAG] [--headed]
//
// On an arm64 Mac the stack runs under amd64 emulation: development evidence
// only; binding acceptance is native linux/amd64.
import {
  zGetPolicyResponse,
  zGetRestoreResponse,
  zGetRunResponse,
  zListPoliciesResponse,
  zListReplicasResponse,
  zListRepositoriesResponse,
  zListSourceChainsResponse,
  zListSourcesResponse,
} from "../lib/api/generated/zod.gen";
import {
  type ApiClient,
  apiLogin,
  Browser,
  expect,
  makeApi,
  runContext,
  stopOnSignal,
  until,
} from "./harness";
import { startStack } from "./stack";

const run = runContext("journey");
const b = new Browser(
  `intern-journey-e2e-${process.pid}`,
  run.evidence,
  run.headed,
);
const longTimeout = 15 * 60_000;

/** Snapshot of the source table; the same query runs on the export. */
const snapshotSql =
  "SELECT COUNT(*), COALESCE(SUM(id), 0), COALESCE(MAX(v), '') FROM journey.t";

/** Opens a Base UI select by its label and picks the option with this text. */
function choose(label: string, option: string): void {
  b.ab("find", "role", "combobox", "click", "--name", label);
  b.waitText(option);
  b.ab("find", "role", "option", "click", "--name", option);
}

/** The id of the detail page the browser is on. */
function detailId(): string {
  const id = new URL(b.url()).searchParams.get("id");
  expect(id !== null && id !== "", `no id in ${b.url()}`);
  return id as string;
}

/** Clicks a trigger on the policy page and follows the run to completed. */
async function triggerFromPolicy(
  api: ApiClient,
  cookie: string,
  origin: string,
  policyId: string,
  button: "立即全量" | "立即增量",
) {
  b.open(`${origin}/policies/detail?id=${policyId}`);
  b.waitText(button);
  b.click(button);
  b.ab("wait", "--url", "**/runs/detail?id=*");
  const runId = detailId();
  b.waitText("已完成", longTimeout);
  const finished = zGetRunResponse.parse(
    (await api("GET", `/api/v1/runs/${runId}`, cookie)).json,
  );
  expect(
    finished.status === "completed" &&
      finished.kind === (button === "立即全量" ? "full" : "incremental") &&
      finished.backup_id !== null,
    `${button} ended ${finished.status}/${finished.kind}`,
  );
  return { runId, backupId: finished.backup_id as string };
}

async function main(): Promise<void> {
  const stack = await startStack(
    run.image === undefined ? undefined : { reuseImage: run.image },
  );
  stopOnSignal(stack.stop);
  console.log(`stack up: ${stack.origin} (${Math.round(process.uptime())}s)`);
  const api = makeApi(stack.origin);
  const admin = await apiLogin(api, stack.admin.username, stack.admin.password);

  try {
    b.signIn(stack.origin, stack.admin.username, stack.admin.password);

    // ------------------------------------------------ J-A1: configuration ---
    b.open(`${stack.origin}/repositories/new`);
    b.waitText("新建仓库");
    b.fill("名称", "journey-local");
    b.click("本地目录", "radio");
    b.fill("根目录", stack.localRepositoryRoot);
    b.click("创建仓库");
    b.waitText("仓库已创建");

    b.open(`${stack.origin}/repositories/new`);
    b.waitText("新建仓库");
    b.fill("名称", "journey-ftp");
    b.click("FTP / FTPS", "radio");
    b.click("明文 FTP", "radio");
    b.fill("主机", stack.ftp.host);
    b.fill("端口", String(stack.ftp.port));
    b.fill("基础路径", stack.ftp.basePath);
    b.fill("用户名", stack.ftp.username);
    b.fill("密码", stack.ftp.password);
    b.click("创建仓库");
    b.waitText("仓库已创建");

    const repositories = zListRepositoriesResponse.parse(
      (await api("GET", "/api/v1/repositories", admin)).json,
    ).items;
    const local = repositories.find((item) => item.name === "journey-local");
    const ftp = repositories.find((item) => item.name === "journey-ftp");
    expect(
      local?.kind === "local" &&
        local.local?.root === stack.localRepositoryRoot,
      `local repository read back as ${JSON.stringify(local)}`,
    );
    expect(
      ftp?.kind === "ftp" &&
        ftp.tls_mode === "plain" &&
        ftp.ftp?.host === stack.ftp.host &&
        ftp.ftp.port === stack.ftp.port &&
        ftp.ftp.base_path === stack.ftp.basePath &&
        ftp.ftp.username === stack.ftp.username &&
        ftp.has_secret,
      `FTP repository read back as ${JSON.stringify(ftp)}`,
    );
    const ftpId = ftp?.id ?? "";
    b.open(`${stack.origin}/repositories/detail?id=${ftpId}`);
    b.waitText("写入性探测");
    b.click("运行探测");
    b.waitText("探测通过");
    b.record({
      id: "01-repositories",
      acceptance: "J-A1",
      description:
        "local and plain FTP repositories created in the UI read back as entered; the FTP write probe passes",
      screenshot: b.shot("01-repositories"),
    });

    b.open(`${stack.origin}/sources/new`);
    b.waitText("新建数据源");
    b.fill("名称", "journey-source");
    b.fill("Socket", stack.mysql.socket);
    b.fill("Datadir", stack.mysql.datadir);
    b.fill("用户名", stack.mysql.username);
    b.fill("密码", stack.mysql.password);
    b.click("创建数据源");
    b.waitText("数据源已创建");
    const source = zListSourcesResponse
      .parse((await api("GET", "/api/v1/sources", admin)).json)
      .items.find((item) => item.name === "journey-source");
    expect(
      source !== undefined &&
        source.socket === stack.mysql.socket &&
        source.datadir === stack.mysql.datadir &&
        source.username === stack.mysql.username &&
        source.has_secret,
      `source read back as ${JSON.stringify(source)}`,
    );
    const sourceId = source?.id ?? "";
    b.open(`${stack.origin}/sources/detail?id=${sourceId}`);
    b.waitText("运行预检");
    b.click("运行预检");
    b.waitText("本次预检结果");
    b.waitText("检查项");
    expect(!b.text().includes("未通过"), "the real preflight did not pass");
    b.record({
      id: "02-source-preflight",
      acceptance: "J-A1",
      description:
        "the source created in the UI reads back as entered and its real preflight against MySQL 8.4.11 + PXB passes",
      screenshot: b.shot("02-source-preflight"),
    });

    b.open(`${stack.origin}/policies/new`);
    b.waitText("创建策略");
    b.fill("名称", "journey-policy");
    choose("数据源", "journey-source");
    choose("本地仓库", "journey-local");
    choose("副本仓库（可选）", "journey-ftp");
    // Far-off schedules: only the buttons on the policy page start backups.
    b.fill("全量 cron", "0 3 1 1 *");
    b.fill("增量 cron", "30 4 1 1 *");
    b.fill("时区", "UTC");
    b.click("创建后立即启用", "switch");
    b.click("创建策略");
    b.ab("wait", "--url", "**/policies/detail?id=*");
    const policyId = detailId();
    b.waitText("已启用");
    const policy = zGetPolicyResponse.parse(
      (await api("GET", `/api/v1/policies/${policyId}`, admin)).json,
    );
    expect(
      policy.name === "journey-policy" &&
        policy.enabled &&
        policy.source_id === sourceId &&
        policy.local_repository_id === local?.id &&
        policy.replica_repository_id === ftpId &&
        policy.full_cron === "0 3 1 1 *" &&
        policy.incremental_cron === "30 4 1 1 *" &&
        policy.timezone === "UTC",
      `policy read back as ${JSON.stringify(policy)}`,
    );
    const policies = zListPoliciesResponse.parse(
      (await api("GET", "/api/v1/policies", admin)).json,
    ).items;
    expect(policies.length === 1, `${policies.length} policies exist`);
    b.record({
      id: "03-policy-enabled",
      acceptance: "J-A1",
      description:
        "the policy created and enabled in the UI (with its real preflight) reads back with the chosen source, repositories and schedules",
      screenshot: b.shot("03-policy-enabled"),
    });

    // ------------------------------------- J-A2: backups and replication ---
    await stack.mysqlQuery(
      "CREATE DATABASE journey; CREATE TABLE journey.t (id INT PRIMARY KEY, v VARCHAR(32)) ENGINE=InnoDB; INSERT INTO journey.t VALUES (1,'full-a'),(2,'full-b'),(3,'full-c');",
    );
    const full = await triggerFromPolicy(
      api,
      admin,
      stack.origin,
      policyId,
      "立即全量",
    );
    b.record({
      id: "04-full-completed",
      acceptance: "J-A2",
      description:
        "立即全量 on the policy page runs a full backup whose live detail ends 已完成, as the API reports",
      screenshot: b.shot("04-full-completed"),
    });
    await stack.mysqlQuery(
      "INSERT INTO journey.t VALUES (4,'inc-a'),(5,'inc-b'); UPDATE journey.t SET v='inc-upd' WHERE id=1; DELETE FROM journey.t WHERE id=2;",
    );
    const snapshot = (await stack.mysqlQuery(snapshotSql)).trim();
    const incremental = await triggerFromPolicy(
      api,
      admin,
      stack.origin,
      policyId,
      "立即增量",
    );
    // A change after the incremental must not reach its restore.
    await stack.mysqlQuery("INSERT INTO journey.t VALUES (6,'after-inc');");
    b.record({
      id: "05-incremental-completed",
      acceptance: "J-A2",
      description:
        "立即增量 on the policy page runs an incremental on the same chain and ends 已完成",
      screenshot: b.shot("05-incremental-completed"),
    });

    const verified = await until(
      "both FTP replicas verified",
      longTimeout,
      async () => {
        const items = zListReplicasResponse
          .parse((await api("GET", "/api/v1/replicas?limit=200", admin)).json)
          .items.filter(
            (item) =>
              item.repository_id === ftpId && item.status === "verified",
          );
        return items.length === 2 ? items : undefined;
      },
    );
    const verifiedBackups = new Set(verified.map((item) => item.backup_id));
    expect(
      verifiedBackups.has(full.backupId) &&
        verifiedBackups.has(incremental.backupId),
      "the verified replicas are not those of the two backups",
    );
    b.open(`${stack.origin}/replicas`);
    b.waitText("已校验");
    const replicasPage = b.text();
    for (const backupId of [full.backupId, incremental.backupId]) {
      expect(
        replicasPage.includes(backupId.slice(0, 8)),
        `the replicas page does not list ${backupId}`,
      );
    }
    b.record({
      id: "06-replicas-verified",
      acceptance: "J-A2",
      description:
        "the replicas page lists both backups' FTP replicas as 已校验, as the API does",
      screenshot: b.shot("06-replicas-verified"),
    });

    // ------------------------------------- J-A3: restore with rehearsal ---
    const chains = zListSourceChainsResponse.parse(
      (await api("GET", `/api/v1/sources/${sourceId}/chains`, admin)).json,
    ).items;
    expect(
      chains.length === 1 && chains[0]?.members.length === 2,
      "expected one chain with the full and the incremental",
    );
    const chainId = chains[0]?.id ?? "";
    b.open(`${stack.origin}/restores/new`);
    b.waitText("新建恢复");
    choose("数据源", "journey-source");
    for (const label of [chainId, "物理增量 2"]) {
      b.ab(
        "wait",
        "--fn",
        `document.querySelector(${JSON.stringify(`[aria-label="${label}"]`)}) !== null`,
      );
      b.ab("find", "role", "radio", "click", "--name", label);
    }
    b.click("生成计划");
    b.waitText("计划摘要");
    b.click("执行演练", "switch");
    b.fill(
      "校验语句（verify_sql）",
      "SELECT COUNT(*) FROM journey.t\nSELECT MAX(v) FROM journey.t",
    );
    b.fill("管理员密码", stack.admin.password);
    b.record({
      id: "07-restore-confirm",
      acceptance: "J-A3",
      description:
        "the wizard's read-only plan for the incremental with the rehearsal and two read-only SELECTs, before the confirmed execution",
      screenshot: b.shot("07-restore-confirm"),
    });
    b.click("执行恢复");
    b.ab("wait", "--url", "**/restores/detail?id=*");
    const restoreId = detailId();
    b.waitText("已完成", longTimeout);
    b.waitText("restore completed");
    b.waitText("导出已发布");
    b.waitText("演练通过");
    const restore = zGetRestoreResponse.parse(
      (await api("GET", `/api/v1/restores/${restoreId}`, admin)).json,
    );
    expect(
      restore.status === "completed" &&
        restore.backup_id === incremental.backupId &&
        restore.verify_status === "passed" &&
        restore.side_effects?.export_published === true &&
        restore.export_path !== null,
      `restore read back as ${restore.status}/${restore.verify_status}/${JSON.stringify(restore.side_effects)}`,
    );
    const server = await stack.startExportedMysql(
      restore.export_path as string,
    );
    let restored: string;
    try {
      restored = (await server.query(snapshotSql)).trim();
    } finally {
      await server.stop();
    }
    expect(
      restored === snapshot,
      `the export holds ${restored}, the snapshot before the incremental is ${snapshot}`,
    );
    b.record({
      id: "08-restore-rehearsed",
      acceptance: "J-A3",
      description:
        "the restore completes with the export published and the rehearsal passed; a fresh mysql:8.4.11 on the export matches the snapshot taken before the incremental",
      screenshot: b.shot("08-restore-rehearsed"),
    });

    b.assertCleanConsole();
    b.record({
      id: "09-clean-console",
      acceptance: "J-A4",
      description: "no page errors or CSP violations across the whole journey",
    });
  } finally {
    b.close();
    await stack.stop();
  }
}

await main().catch((error: unknown) => {
  b.close();
  throw error;
});
b.writeManifest(run.commit, run.dirty);
console.log(
  `journey e2e: ${b.steps.length} steps passed; evidence in ${run.evidence}${run.dirty ? " (working tree dirty)" : ""}`,
);
