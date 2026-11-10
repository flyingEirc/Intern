// F4 (#58) acceptance through the real UI: the restore wizard, second
// confirmation, plan_changed, local and FTP-only restores of incremental 1
// and 2 compared with independent snapshots on a brand-new mysql:8.4.11, the
// rehearsal with passing and failing verify_sql, cancellation side effects
// checked against the export root, roles, PITR shown unavailable and the 429
// confirmation limit. Real components only (e2e/stack.ts).
//
//   bun e2e/f4.ts [--evidence DIR] [--image TAG] [--headed]
//
// On an arm64 Mac the stack runs under amd64 emulation: development evidence
// only; binding acceptance is native linux/amd64.
import { randomUUID } from "node:crypto";
import { z } from "zod";
import {
  zGetBackupResponse,
  zGetRestoreResponse,
  zListSourceChainsResponse,
  zPlanRestoreResponse,
  zReplica,
  zRun,
} from "../lib/api/generated/zod.gen";
import {
  type ApiClient,
  apiLogin,
  Browser,
  createUser,
  expect,
  makeApi,
  randomPassword,
  runContext,
  stopOnSignal,
  until,
} from "./harness";
import { type Stack, startStack } from "./stack";

const run = runContext("f4");
const b = new Browser(`intern-f4-e2e-${process.pid}`, run.evidence, run.headed);
const longTimeout = 15 * 60_000;

function createdId(
  what: string,
  result: { status: number; json: unknown },
): string {
  expect(
    result.status === 201,
    `${what} = ${result.status} ${JSON.stringify(result.json)}`,
  );
  return z.object({ id: z.string() }).parse(result.json).id;
}

async function backupRun(
  api: ApiClient,
  cookie: string,
  policyId: string,
  kind: "full" | "incremental",
) {
  const triggered = await api(
    "POST",
    `/api/v1/policies/${policyId}/runs`,
    cookie,
    { kind },
    { "Idempotency-Key": randomUUID() },
  );
  expect(triggered.status === 202, `trigger ${kind} = ${triggered.status}`);
  const id = z.object({ run_id: z.string() }).parse(triggered.json).run_id;
  const finished = await until(`${kind} run`, longTimeout, async () => {
    const current = zRun.parse(
      (await api("GET", `/api/v1/runs/${id}`, cookie)).json,
    );
    return ["completed", "failed", "canceled"].includes(current.status)
      ? current
      : undefined;
  });
  expect(
    finished.status === "completed" && finished.backup_id !== null,
    `${kind} ended ${finished.status}`,
  );
  return finished.backup_id as string;
}

async function restoreRow(api: ApiClient, cookie: string, id: string) {
  return zGetRestoreResponse.parse(
    (await api("GET", `/api/v1/restores/${id}`, cookie)).json,
  );
}

async function waitRestore(api: ApiClient, cookie: string, id: string) {
  return until(`restore ${id}`, longTimeout, async () => {
    const row = await restoreRow(api, cookie, id);
    return ["completed", "failed", "canceled"].includes(row.status)
      ? row
      : undefined;
  });
}

/** Snapshot of the source table at one point; the same query runs on every export. */
const snapshotSql =
  "SELECT COUNT(*), COALESCE(SUM(id), 0), COALESCE(MAX(v), '') FROM f4.t";

async function exportSnapshot(
  stack: Stack,
  exportPath: string,
): Promise<string> {
  const server = await stack.startExportedMysql(exportPath);
  try {
    return (await server.query(snapshotSql)).trim();
  } finally {
    await server.stop();
  }
}

/** Walks the wizard to one backup point of the source. */
function choosePoint(sourceName: string, backupId: string): void {
  b.waitText("新建恢复");
  b.ab("find", "role", "combobox", "click", "--name", "数据源");
  b.waitText(sourceName);
  b.ab("find", "role", "option", "click", "--name", sourceName);
  // A single chain is selected automatically; wait for its members to load.
  b.waitText("备份组：自");
  b.ab(
    "wait",
    "--fn",
    `document.querySelector(${JSON.stringify(`input[type="radio"][value="${backupId}"]`)}) !== null`,
  );
  // Points of one kind share their completion minute, so choose by id.
  // Base UI renders the value on a hidden input next to the visible radio.
  const clicked = b.ab(
    "eval",
    `(() => { const input = document.querySelector(${JSON.stringify(`input[type="radio"][value="${backupId}"]`)}); const target = input?.previousElementSibling; if (target?.getAttribute("role") !== "radio") return false; target.click(); return true; })()`,
  );
  expect(
    clicked === "true",
    `missing point ${backupId}; page says: ${b.text().slice(0, 2000)}`,
  );
  b.ab(
    "wait",
    "--fn",
    `document.querySelector(${JSON.stringify(`input[type="radio"][value="${backupId}"]`)})?.previousElementSibling?.getAttribute("aria-checked") === "true"`,
  );
  b.click("生成计划");
  // The plan card renders before the plan arrives; wait for its capacity lines.
  b.waitText("导出空间");
}

function confirmAndExecute(password: string): void {
  b.fill("管理员密码", password);
  b.click("执行恢复");
}

async function main(): Promise<void> {
  const stack = await startStack(
    run.image === undefined ? undefined : { reuseImage: run.image },
  );
  stopOnSignal(stack.stop);
  console.log(`stack up: ${stack.origin} (${Math.round(process.uptime())}s)`);
  const api = makeApi(stack.origin);
  const admin = await apiLogin(api, stack.admin.username, stack.admin.password);
  const operator = {
    username: "f4-operator",
    password: randomPassword("operator"),
  };
  const viewer = { username: "f4-viewer", password: randomPassword("viewer") };
  await createUser(
    api,
    admin,
    operator.username,
    operator.password,
    "operator",
  );
  await createUser(api, admin, viewer.username, viewer.password, "viewer");

  try {
    // ---------------------------------------------------- backups and data ---
    const localId = createdId(
      "local repository",
      await api("POST", "/api/v1/repositories", admin, {
        name: "f4-local",
        kind: "local",
        tls_mode: "none",
        root: stack.localRepositoryRoot,
      }),
    );
    const ftpId = createdId(
      "ftp repository",
      await api("POST", "/api/v1/repositories", admin, {
        name: "f4-ftp",
        kind: "ftp",
        tls_mode: "plain",
        ftp: {
          host: stack.ftp.host,
          port: stack.ftp.port,
          base_path: stack.ftp.basePath,
          username: stack.ftp.username,
        },
        password: stack.ftp.password,
      }),
    );
    const sourceId = createdId(
      "source",
      await api("POST", "/api/v1/sources", admin, {
        name: "f4-source",
        socket: stack.mysql.socket,
        datadir: stack.mysql.datadir,
        username: stack.mysql.username,
        password: stack.mysql.password,
      }),
    );
    const policyId = createdId(
      "policy",
      await api("POST", "/api/v1/policies", admin, {
        name: "f4-policy",
        source_id: sourceId,
        local_repository_id: localId,
        replica_repository_id: ftpId,
        full_cron: "0 3 1 1 *",
        incremental_cron: "30 4 1 * *",
        timezone: "UTC",
        enabled: false,
      }),
    );
    const enabled = await api(
      "POST",
      `/api/v1/policies/${policyId}/enable`,
      admin,
    );
    expect(enabled.status === 200, `enable = ${enabled.status}`);

    await stack.mysqlQuery(
      "CREATE DATABASE f4; CREATE TABLE f4.t (id INT PRIMARY KEY, v VARCHAR(32)) ENGINE=InnoDB; INSERT INTO f4.t VALUES (1,'full-a'),(2,'full-b'),(3,'full-c');",
    );
    const fullId = await backupRun(api, admin, policyId, "full");
    await stack.mysqlQuery(
      "INSERT INTO f4.t VALUES (4,'inc1-a'),(5,'inc1-b'); UPDATE f4.t SET v='inc1-upd' WHERE id=1;",
    );
    const snapshot1 = (await stack.mysqlQuery(snapshotSql)).trim();
    const inc1Id = await backupRun(api, admin, policyId, "incremental");
    await stack.mysqlQuery(
      "INSERT INTO f4.t VALUES (6,'inc2-a'); DELETE FROM f4.t WHERE id=2; UPDATE f4.t SET v='zz-inc2' WHERE id=3;",
    );
    const snapshot2 = (await stack.mysqlQuery(snapshotSql)).trim();
    const inc2Id = await backupRun(api, admin, policyId, "incremental");
    // Later changes must not appear in any restore of incremental 1 or 2.
    await stack.mysqlQuery("INSERT INTO f4.t VALUES (7,'after-inc2');");
    expect(snapshot1 !== snapshot2, "the two snapshots must differ");
    await until("three verified replicas", longTimeout, async () => {
      const items = z
        .object({ items: z.array(zReplica) })
        .parse((await api("GET", "/api/v1/replicas?limit=200", admin)).json)
        .items.filter(
          (item) => item.repository_id === ftpId && item.status === "verified",
        );
      return items.length === 3 ? items : undefined;
    });
    const chains = zListSourceChainsResponse.parse(
      (await api("GET", `/api/v1/sources/${sourceId}/chains`, admin)).json,
    );
    expect(
      chains.items.length === 1 && chains.items[0]?.members.length === 3,
      "expected one 3-member chain",
    );
    console.log(
      `backups ready: full ${fullId}, inc1 ${inc1Id}, inc2 ${inc2Id}`,
    );

    // --- F4-A5: PITR is visibly unavailable ---
    b.signIn(stack.origin, stack.admin.username, stack.admin.password);
    b.open(`${stack.origin}/restores/new`);
    b.waitText("任意时间点恢复（暂不支持）");
    const pitrDisabled = b.ab(
      "eval",
      `document.querySelector('[aria-label="任意时间点恢复（暂不支持）"]')?.hasAttribute("data-disabled") || document.querySelector('[aria-label="任意时间点恢复（暂不支持）"]')?.getAttribute("aria-disabled") === "true" || document.querySelector('[aria-label="任意时间点恢复（暂不支持）"]')?.hasAttribute("disabled")`,
    );
    expect(pitrDisabled === "true", "the PITR option is not disabled");
    b.record({
      id: "01-pitr-unavailable",
      acceptance: "F4-A5",
      description:
        "the wizard offers completed-backup restore only and explains that arbitrary point-in-time recovery is unavailable",
      screenshot: b.shot("01-pitr-unavailable"),
    });

    // --- F4-A1 (local) + F4-A2 (wrong password): restore incremental 1 ---
    choosePoint("f4-source", inc1Id);
    const plan1 = zPlanRestoreResponse.parse(
      (await api("POST", "/api/v1/restores/plan", admin, { backup_id: inc1Id }))
        .json,
    );
    expect(
      plan1.links.length === 2 &&
        plan1.links.every((link) => link.source === "local"),
      `incremental 1 plan sources ${JSON.stringify(plan1.links.map((link) => link.source))}`,
    );
    b.record({
      id: "02-plan-local",
      acceptance: "F4-A1",
      description:
        "the read-only plan for incremental 1 lists both links from the local repository with space, export path and digest",
      screenshot: b.shot("02-plan-local"),
    });
    confirmAndExecute("not-the-admin-password");
    b.waitText("密码确认失败，请重新输入。");
    const noRestoreYet = z
      .object({ items: z.array(z.object({ id: z.string() })) })
      .parse((await api("GET", "/api/v1/restores", admin)).json);
    expect(
      noRestoreYet.items.length === 0,
      "a wrong password created a restore",
    );
    b.record({
      id: "03-confirmation-failed",
      acceptance: "F4-A2",
      description:
        "a wrong confirmation password is refused (403) and no restore is created",
      screenshot: b.shot("03-confirmation-failed"),
    });
    confirmAndExecute(stack.admin.password);
    b.ab("wait", "--url", "**/restores/detail?id=*");
    const restore1Id = new URL(b.url()).searchParams.get("id") ?? "";
    b.waitText("已完成", longTimeout);
    // The log written at the terminal transition must reach the page too.
    b.waitText("restore completed");
    b.waitText("导出已发布");
    const restore1 = await restoreRow(api, admin, restore1Id);
    expect(
      restore1.status === "completed" && restore1.export_path !== null,
      `restore 1 ${restore1.status}`,
    );
    expect(
      (await exportSnapshot(stack, restore1.export_path as string)) ===
        snapshot1,
      "incremental 1 export differs from its snapshot",
    );
    b.record({
      id: "04-restore-inc1-local",
      acceptance: "F4-A1",
      description:
        "incremental 1 restored from local payloads; a fresh mysql:8.4.11 on the export matches the snapshot taken before incremental 1",
      screenshot: b.shot("04-restore-inc1-local"),
    });

    // --- F4-A2 (plan_changed) + F4-A1 (FTP only): restore incremental 2 ---
    b.open(`${stack.origin}/restores/new`);
    choosePoint("f4-source", inc2Id);
    // The catalog changes under the confirmed plan: every local payload goes.
    for (const id of [fullId, inc1Id, inc2Id]) {
      const backup = zGetBackupResponse.parse(
        (await api("GET", `/api/v1/backups/${id}`, admin)).json,
      );
      stack.removeLocalPayload(backup.object_path);
    }
    confirmAndExecute(stack.admin.password);
    b.waitText("目录已变化");
    b.record({
      id: "05-plan-changed",
      acceptance: "F4-A2",
      description:
        "removing the local payloads after planning makes the confirmed digest stale: 409 plan_changed, re-plan required",
      screenshot: b.shot("05-plan-changed"),
    });
    b.click("重新生成计划");
    b.waitText("导出空间");
    const plan2 = zPlanRestoreResponse.parse(
      (await api("POST", "/api/v1/restores/plan", admin, { backup_id: inc2Id }))
        .json,
    );
    expect(
      plan2.links.length === 3 &&
        plan2.links.every((link) => link.source === "ftp"),
      `incremental 2 plan sources ${JSON.stringify(plan2.links.map((link) => link.source))}`,
    );
    confirmAndExecute(stack.admin.password);
    b.ab("wait", "--url", "**/restores/detail?id=*");
    const restore2Id = new URL(b.url()).searchParams.get("id") ?? "";
    b.waitText("已完成", longTimeout);
    // The log written at the terminal transition must reach the page too.
    b.waitText("restore completed");
    const restore2 = await restoreRow(api, admin, restore2Id);
    expect(
      restore2.status === "completed" && restore2.export_path !== null,
      `restore 2 ${restore2.status}`,
    );
    expect(
      (await exportSnapshot(stack, restore2.export_path as string)) ===
        snapshot2,
      "incremental 2 export differs from its snapshot",
    );
    b.record({
      id: "06-restore-inc2-ftp",
      acceptance: "F4-A1",
      description:
        "with no local payload left, incremental 2 is restored from FTP replicas only; the fresh mysql:8.4.11 matches the snapshot before incremental 2",
      screenshot: b.shot("06-restore-inc2-ftp"),
    });

    // --- F4-A4: rehearsal, passing and failing verify_sql ---
    b.open(`${stack.origin}/restores/new`);
    choosePoint("f4-source", inc1Id);
    b.click("执行演练", "switch");
    b.fill("校验语句", Array.from({ length: 17 }, () => "SELECT 1").join("\n"));
    confirmAndExecute(stack.admin.password);
    b.waitText("校验语句最多 16 条");
    const stillTwo = z
      .object({ items: z.array(z.object({ id: z.string() })) })
      .parse((await api("GET", "/api/v1/restores", admin)).json);
    expect(
      stillTwo.items.length === 2,
      "17 verify statements reached the server",
    );
    b.fill("校验语句", "SELECT COUNT(*) FROM f4.t\nSELECT MAX(v) FROM f4.t");
    confirmAndExecute(stack.admin.password);
    b.ab("wait", "--url", "**/restores/detail?id=*");
    const rehearsedId = new URL(b.url()).searchParams.get("id") ?? "";
    b.waitText("演练通过", longTimeout);
    const rehearsed = await restoreRow(api, admin, rehearsedId);
    expect(
      rehearsed.verify_status === "passed",
      `rehearsal ${rehearsed.verify_status}`,
    );
    b.record({
      id: "07-rehearsal-passed",
      acceptance: "F4-A4",
      description:
        "17 statements are refused before submit; a rehearsal with two read-only SELECTs passes and shows each result",
      screenshot: b.shot("07-rehearsal-passed"),
    });

    b.open(`${stack.origin}/restores/new`);
    choosePoint("f4-source", inc1Id);
    b.click("执行演练", "switch");
    b.fill("校验语句", "SELECT COUNT(*) FROM f4.missing_table");
    confirmAndExecute(stack.admin.password);
    b.ab("wait", "--url", "**/restores/detail?id=*");
    const failedVerifyId = new URL(b.url()).searchParams.get("id") ?? "";
    b.waitText("演练失败", longTimeout);
    const failedVerify = await restoreRow(api, admin, failedVerifyId);
    expect(
      failedVerify.verify_status === "failed",
      `failing rehearsal ${failedVerify.verify_status}`,
    );
    b.record({
      id: "08-rehearsal-failed",
      acceptance: "F4-A4",
      description:
        "a verify_sql query on a missing table fails the rehearsal, and the page reports 演练失败 with the statement result",
      screenshot: b.shot("08-rehearsal-failed"),
    });

    // --- F4-A3: cancel while running; side effects match the export root ---
    b.open(`${stack.origin}/restores/new`);
    choosePoint("f4-source", inc2Id);
    b.click("执行演练", "switch");
    confirmAndExecute(stack.admin.password);
    b.ab("wait", "--url", "**/restores/detail?id=*");
    const cancelId = new URL(b.url()).searchParams.get("id") ?? "";
    // Cancel while the engine extracts or prepares, not while the row is still
    // queued or authenticating: that is the interruption (F4-A3) whose side
    // effects the page must state.
    const running = await until(
      "the restore to extract or prepare",
      longTimeout,
      async () => {
        const row = await restoreRow(api, admin, cancelId);
        if (row.status !== "running") {
          expect(
            row.status === "queued",
            `restore ended ${row.status} before it could be canceled`,
          );
          return undefined;
        }
        return row.phase === "extracting" || row.phase === "preparing"
          ? row
          : undefined;
      },
    );
    b.waitText("执行中");
    b.click("取消恢复");
    b.waitText("取消这次恢复？");
    b.click("确认取消");
    const canceled = await waitRestore(api, admin, cancelId);
    expect(
      canceled.status === "canceled",
      `a restore canceled in phase ${running.phase} ended ${canceled.status}`,
    );
    b.waitText("已取消", longTimeout);
    const effects = canceled.side_effects;
    expect(effects !== null, "a terminal restore has no side_effects");
    const entries = stack.exportEntries();
    const published = entries.includes(cancelId);
    expect(
      published === (effects?.export_published === true),
      `export_published=${effects?.export_published} but the export root holds ${JSON.stringify(entries)}`,
    );
    for (const statement of [
      effects?.work_dir_removed ? "工作目录已删除" : "工作目录未删除",
      effects?.pending_export_removed ? "待发布导出已删除" : "待发布导出未删除",
      effects?.export_published ? "导出已发布" : "导出未发布",
    ]) {
      b.waitText(statement);
    }
    expect(!b.text().includes("回滚"), "the restore page describes a rollback");
    b.record({
      id: "09-restore-canceled",
      acceptance: "F4-A3",
      description: `a restore canceled in phase ${running.phase} states all three side effects; the export root agrees with export_published=${effects?.export_published}`,
      screenshot: b.shot("09-restore-canceled"),
    });

    // --- F4-A5: operator plans only, viewer reads only ---
    b.signOut();
    b.signIn(stack.origin, operator.username, operator.password);
    b.open(`${stack.origin}/restores/new`);
    choosePoint("f4-source", inc1Id);
    b.waitText("只有管理员可以执行恢复");
    const operatorCookie = await apiLogin(
      api,
      operator.username,
      operator.password,
    );
    const operatorExecute = await api(
      "POST",
      "/api/v1/restores",
      operatorCookie,
      {
        backup_id: inc1Id,
        plan_digest: plan1.plan_digest,
        password: operator.password,
        rehearse: false,
        verify_sql: [],
      },
    );
    expect(
      operatorExecute.status === 403,
      `operator execute = ${operatorExecute.status}`,
    );
    b.record({
      id: "10-operator-plans-only",
      acceptance: "F4-A5",
      description:
        "an operator gets the read-only plan but no confirm step, and the execute API answers 403",
      screenshot: b.shot("10-operator-plans-only"),
    });
    b.signOut();
    b.signIn(stack.origin, viewer.username, viewer.password);
    b.open(`${stack.origin}/restores`);
    b.waitText("已完成");
    b.open(`${stack.origin}/restores/new`);
    b.waitText("只有操作员或管理员可以计划恢复");
    b.record({
      id: "11-viewer-read-only",
      acceptance: "F4-A5",
      description: "a viewer reads the restore list but cannot plan",
      screenshot: b.shot("11-viewer-read-only"),
    });

    // --- F4-A2: the per-user confirmation limit answers 429 with Retry-After ---
    b.signOut();
    b.signIn(stack.origin, stack.admin.username, stack.admin.password);
    b.open(`${stack.origin}/restores/new`);
    choosePoint("f4-source", inc1Id);
    const freshAdmin = await apiLogin(
      api,
      stack.admin.username,
      stack.admin.password,
    );
    const plan = zPlanRestoreResponse.parse(
      (
        await api("POST", "/api/v1/restores/plan", freshAdmin, {
          backup_id: inc1Id,
        })
      ).json,
    );
    for (let attempt = 0; attempt < 5; attempt++) {
      await api("POST", "/api/v1/restores", freshAdmin, {
        backup_id: inc1Id,
        plan_digest: plan.plan_digest,
        password: "wrong-password-to-spend-the-budget",
        rehearse: false,
        verify_sql: [],
      });
    }
    confirmAndExecute(stack.admin.password);
    b.waitText("密码确认尝试过于频繁");
    expect(
      /请 \d+ 秒后再试/.test(b.text()),
      "the 429 notice does not show the Retry-After seconds",
    );
    b.record({
      id: "12-confirmation-rate-limited",
      acceptance: "F4-A2",
      description:
        "after five wrong confirmations in a minute the next one is 429 and the page shows the Retry-After wait",
      screenshot: b.shot("12-confirmation-rate-limited"),
    });

    b.assertCleanConsole();
    b.record({
      id: "13-clean-console",
      acceptance: "F4-A6",
      description: "no page errors or CSP violations",
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
  `f4 e2e: ${b.steps.length} steps passed; evidence in ${run.evidence}${run.dirty ? " (working tree dirty)" : ""}`,
);
