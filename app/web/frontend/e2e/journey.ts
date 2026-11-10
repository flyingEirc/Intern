// #65 (stage F, #19 acceptance 3): the whole business flow through the UI
// only, on the real Compose stack: repositories, source and preflight, policy
// enabled with an FTP replica, full and two incremental backups triggered from
// the policy page, all three FTP replicas verified, then the selected second
// incremental restored with a rehearsal and compared with an independent snapshot
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
const keepStack = process.argv.includes("--keep");
let completed = false;

/** Snapshot of the source table; the same query runs on the export. */
const snapshotSql =
  "SELECT COUNT(*), COALESCE(SUM(id), 0), COALESCE(MAX(v), '') FROM journey.t";

/** Uses keyboard selection; labels are read back from the real option list. */
function choose(label: string, option: string): void {
  const id = JSON.parse(
    b.ab(
      "eval",
      `Array.from(document.querySelectorAll('label')).find((element) => element.textContent.trim() === ${JSON.stringify(label)})?.htmlFor`,
    ),
  ) as string;
  expect(Boolean(id), `missing control label ${label}`);
  b.ab("focus", `#${id}`);
  b.ab("press", "Space");
  b.ab(
    "wait",
    "--fn",
    `document.getElementById(${JSON.stringify(id)})?.getAttribute('aria-expanded') === 'true'`,
  );
  const popupId = b.ab("get", "attr", `#${id}`, "aria-controls");
  const popup = `[id=${JSON.stringify(popupId)}]`;
  b.ab("wait", `${popup} [role="option"]`);
  const labels = JSON.parse(
    b.ab(
      "eval",
      `Array.from(document.querySelectorAll(${JSON.stringify(`${popup} [role="option"]`)}), (element) => element.textContent.trim())`,
    ),
  ) as string[];
  const index = labels.indexOf(option);
  expect(index >= 0, `missing option ${option}`);
  b.ab("press", "Home");
  for (let cursor = 0; cursor < index; cursor += 1) b.ab("press", "ArrowDown");
  b.ab("press", "Enter");
  b.ab(
    "wait",
    "--fn",
    `document.getElementById(${JSON.stringify(id)})?.getAttribute('aria-expanded') === 'false' && (document.getElementById(${JSON.stringify(popupId)})?.getBoundingClientRect().height ?? 0) === 0`,
  );
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
  const stack = await startStack({
    ...(run.image === undefined ? {} : { reuseImage: run.image }),
    setupInBrowser: true,
  });
  stopOnSignal(stack.stop);
  console.log(`stack up: ${stack.origin} (${Math.round(process.uptime())}s)`);
  const api = makeApi(stack.origin);

  try {
    b.open(`${stack.origin}/login`);
    b.waitText("创建首位管理员");
    const setupShots = b.responsive("U2-setup");
    b.record({
      id: "U2-setup",
      acceptance: "U2",
      description:
        "the first-run setup page at 375, 768 and 1440 logical pixels, before any administrator exists",
      screenshots: setupShots,
    });
    b.ab("focus", "#setup-username");
    b.ab("type", ":focus", stack.admin.username);
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "id") === "setup-password",
      "setup Tab missed password",
    );
    b.ab("type", ":focus", stack.admin.password);
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "id") === "setup-confirm",
      "setup Tab missed confirmation",
    );
    b.ab("type", ":focus", stack.admin.password);
    b.ab("press", "Tab");
    expect(
      b.ab("get", "text", ":focus") === "创建管理员并进入控制台",
      "setup Tab missed submit",
    );
    b.record({
      id: "U3-setup-keyboard",
      acceptance: "U3",
      description:
        "setup username, password and confirmation entered with keyboard typing and Tab; focus reaches the submit button before Enter creates the administrator",
      screenshot: b.shot("U3-setup-keyboard"),
    });
    b.ab("press", "Enter");
    b.waitText("数据保护概览");
    const admin = await apiLogin(
      api,
      stack.admin.username,
      stack.admin.password,
    );

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
    b.fill("连接路径（Unix socket）", stack.mysql.socket);
    b.fill("数据目录", stack.mysql.datadir);
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
    b.ab("focus", "#policy-name");
    b.ab("type", ":focus", "journey-policy");
    choose("数据源", "journey-source");
    choose("本地仓库", "journey-local");
    choose("副本仓库（可选）", "journey-ftp");
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "aria-label") === "创建后立即启用",
      "policy Tab missed enabled switch",
    );
    b.ab("press", "Space");
    // Far-off schedules: only the buttons on the policy page start backups.
    b.ab("focus", '[aria-label="全量保护频率"] button:last-child');
    b.ab("press", "Space");
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "id") === "full-cron",
      "policy Tab missed custom full cron",
    );
    b.ab("press", "Home");
    b.ab("press", "Shift+End");
    b.ab("type", ":focus", "0 3 1 1 *");
    b.ab("focus", '[aria-label="增量保护频率"] button:last-child');
    b.ab("press", "Space");
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "id") === "incremental-cron",
      "policy Tab missed custom incremental cron",
    );
    b.ab("press", "Home");
    b.ab("press", "Shift+End");
    b.ab("type", ":focus", "30 4 1 1 *");
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "id") === "policy-timezone",
      "policy Tab missed timezone",
    );
    b.ab("press", "Home");
    b.ab("press", "Shift+End");
    b.ab("type", ":focus", "UTC");
    b.record({
      id: "U2-policy-create-filled",
      acceptance: "U2",
      description:
        "the populated policy creation form with source, local/FTP repositories, custom schedules and enabled state at 375, 768 and 1440 pixels",
      screenshots: b.responsive("U2-policy-create-filled"),
    });
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "type") === "submit",
      "policy Tab missed submit",
    );
    b.record({
      id: "U3-policy-keyboard",
      acceptance: "U3",
      description:
        "policy name, combobox options, enable switch, custom frequency toggles, cron and timezone entered with keyboard events; Tab reaches submit before Enter, with API readback below",
      screenshot: b.shot("U3-policy-keyboard"),
    });
    b.ab("press", "Enter");
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
    const firstIncremental = await triggerFromPolicy(
      api,
      admin,
      stack.origin,
      policyId,
      "立即增量",
    );
    await stack.mysqlQuery(
      "INSERT INTO journey.t VALUES (6,'inc-two'); UPDATE journey.t SET v='inc-two-upd' WHERE id=3;",
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
    await stack.mysqlQuery("INSERT INTO journey.t VALUES (7,'after-inc');");
    b.record({
      id: "05-incremental-completed",
      acceptance: "J-A2",
      description:
        "two successive 立即增量 actions complete on the same chain, preserving the snapshot of the selected second point",
      screenshot: b.shot("05-incremental-completed"),
    });

    const verified = await until(
      "all three FTP replicas verified",
      longTimeout,
      async () => {
        const items = zListReplicasResponse
          .parse((await api("GET", "/api/v1/replicas?limit=200", admin)).json)
          .items.filter(
            (item) =>
              item.repository_id === ftpId && item.status === "verified",
          );
        return items.length === 3 ? items : undefined;
      },
    );
    const verifiedBackups = new Set(verified.map((item) => item.backup_id));
    expect(
      verifiedBackups.has(full.backupId) &&
        verifiedBackups.has(firstIncremental.backupId) &&
        verifiedBackups.has(incremental.backupId),
      "the verified replicas are not those of all three backups",
    );
    b.open(`${stack.origin}/replicas`);
    b.waitText("已校验");
    const replicaTargets = b.ab(
      "eval",
      "JSON.stringify(Array.from(document.querySelectorAll('a[href*=\"/backups/detail?id=\"]'), (element) => element.getAttribute('href')))",
    );
    for (const backupId of [
      full.backupId,
      firstIncremental.backupId,
      incremental.backupId,
    ]) {
      expect(
        replicaTargets.includes(backupId),
        `the replicas page does not link ${backupId}`,
      );
    }
    b.record({
      id: "06-replicas-verified",
      acceptance: "J-A2",
      description:
        "the replicas page lists all three backups' FTP replicas as 已校验, as the API does",
      screenshot: b.shot("06-replicas-verified"),
    });

    // ------------------------------------- J-A3: restore with rehearsal ---
    const chains = zListSourceChainsResponse.parse(
      (await api("GET", `/api/v1/sources/${sourceId}/chains`, admin)).json,
    ).items;
    expect(
      chains.length === 1 && chains[0]?.members.length === 3,
      "expected one chain with the full and two incrementals",
    );
    b.open(`${stack.origin}/restores/new`);
    b.waitText("新建恢复");
    choose("数据源", "journey-source");
    b.waitText("备份组：自");
    // Chain members are full, then the two incrementals. Point labels are
    // "物理增量 · <time>" and the two incrementals can share a minute, so
    // select the third member by its backup id rather than the label.
    const pointId = chains[0]?.members[2]?.id ?? "";
    expect(pointId !== "", "the chain has no third backup point");
    b.ab(
      "wait",
      "--fn",
      `document.querySelector(${JSON.stringify(`input[type="radio"][value="${pointId}"]`)}) !== null`,
    );
    b.ab("focus", '[aria-label="备份点"] tbody tr:first-child [role="radio"]');
    b.ab("press", "Space");
    b.ab("press", "ArrowDown");
    b.ab("press", "ArrowDown");
    b.ab(
      "wait",
      "--fn",
      `document.querySelector(${JSON.stringify(`input[type="radio"][value="${pointId}"]`)})?.previousElementSibling?.getAttribute("aria-checked") === "true"`,
    );
    b.ab("press", "Tab");
    expect(
      b.ab("get", "text", ":focus") === "生成计划",
      "restore Tab missed plan button",
    );
    b.ab("press", "Enter");
    b.waitText("导出空间");
    b.ab("focus", "#restore-password");
    b.ab("type", ":focus", stack.admin.password);
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "aria-label") === "执行演练",
      "restore Tab missed rehearsal switch",
    );
    b.ab("press", "Space");
    b.ab("press", "Tab");
    expect(
      b.ab("get", "attr", ":focus", "id") === "restore-verify-sql",
      "restore Tab missed verify SQL",
    );
    b.ab(
      "type",
      ":focus",
      "SELECT COUNT(*) FROM journey.t\nSELECT MAX(v) FROM journey.t",
    );
    b.record({
      id: "07-restore-confirm",
      acceptance: "J-A3",
      description:
        "the wizard's read-only plan for the incremental with the rehearsal and two read-only SELECTs, before the confirmed execution",
      screenshot: b.shot("07-restore-confirm"),
    });
    b.ab("press", "Tab");
    expect(
      b.ab("get", "text", ":focus") === "执行恢复",
      "restore Tab missed confirmation button",
    );
    b.record({
      id: "U3-restore-keyboard",
      acceptance: "U3",
      description:
        "source chosen with keyboard; backup radio selected with Space/ArrowDown, Tab/Enter generates plan; password, rehearsal and SQL use typing/Tab/Space; Tab reaches final confirmation before Enter",
      screenshot: b.shot("U3-restore-keyboard"),
    });
    b.ab("press", "Enter");
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

    // ------------------------------------- U2: console layout evidence ---
    const pages: Array<[string, string, string]> = [
      ["overview", "/", "数据保护概览"],
      ["sources", "/sources", "数据源"],
      ["source-new", "/sources/new", "新建数据源"],
      ["repositories", "/repositories", "备份仓库"],
      ["repository-new", "/repositories/new", "新建仓库"],
      ["policies", "/policies", "保护策略"],
      ["policy-new", "/policies/new", "新建策略"],
      ["policy-detail", `/policies/detail?id=${policyId}`, "调度预览"],
      ["policy-edit", `/policies/new?id=${policyId}`, "保存修改"],
      ["runs", "/runs", "运行记录"],
      ["run-detail", `/runs/detail?id=${full.runId}`, "日志"],
      ["replicas", "/replicas", "异地副本"],
      ["restores", "/restores", "新建恢复"],
      ["restore-wizard", "/restores/new", "新建恢复"],
      ["restore-detail", `/restores/detail?id=${restoreId}`, "恢复详情"],
      ["users", "/users", "用户列表"],
      ["audit", "/audit", "审计记录"],
      ["account", "/account", "修改密码"],
      ["backup-detail", `/backups/detail?id=${full.backupId}`, "备份详情"],
    ];
    for (const [name, route, text] of pages) {
      b.open(`${stack.origin}${route}`);
      b.waitText(text);
      const shots = b.responsive(`U2-${name}`);
      b.record({
        id: `U2-${name}`,
        acceptance: "U2",
        description: `${route} at 375, 768 and 1440 logical pixels; no page-level horizontal overflow`,
        screenshots: shots,
      });
    }
    b.signOut();
    const loginShots = b.responsive("U2-login");
    b.record({
      id: "U2-login",
      acceptance: "U2",
      description:
        "the initialized sign-in page at 375, 768 and 1440 logical pixels; it never offers setup again",
      screenshots: loginShots,
    });
    b.assertCleanConsole();
    completed = true;
    b.record({
      id: "09-clean-console",
      acceptance: "J-A4",
      description: "no page errors or CSP violations across the whole journey",
    });
  } catch (error) {
    b.record({
      id: "failure",
      acceptance: "diagnostic",
      description:
        "the first failed step; see the process error for its assertion",
      screenshot: b.shot("failure"),
    });
    b.writeManifest(run.commit, run.dirty);
    throw error;
  } finally {
    if (keepStack && completed) {
      b.signIn(stack.origin, stack.admin.username, stack.admin.password);
      b.open(`${stack.origin}/`);
      b.waitText("数据保护概览");
      const state = `${run.evidence}/authenticated-state.json`;
      b.ab("state", "save", state);
      console.log(`REVIEW_READY ${stack.origin} STATE ${state}`);
      b.writeManifest(run.commit, run.dirty);
      await new Promise(() => {});
    }
    b.close();
    if (!keepStack) await stack.stop();
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
