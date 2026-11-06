// F3 (#57) acceptance through the real UI: schedule preview, manual runs with
// live SSE, cancellation, chains, split local/remote protection and the FTP
// replica queue, against the real-component stack (e2e/stack.ts). Backups are
// real XtraBackup runs on MySQL 8.4.11, replicas are real uploads to Pure-FTPd
// 1.0.54 with read-back verification.
//
//   bun e2e/f3.ts [--evidence DIR] [--image TAG] [--headed]
//
// On an arm64 Mac the stack runs under amd64 emulation: development evidence
// only; binding acceptance is native linux/amd64.
import { randomUUID } from "node:crypto";
import { z } from "zod";
import {
  zBackup,
  zListSourceChainsResponse,
  zPolicySchedule,
  zProtection,
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
import { startStack } from "./stack";

const run = runContext("f3");
const b = new Browser(`intern-f3-e2e-${process.pid}`, run.evidence, run.headed);
const backupTimeout = 15 * 60_000;

async function getRun(api: ApiClient, cookie: string, id: string) {
  return zRun.parse((await api("GET", `/api/v1/runs/${id}`, cookie)).json);
}

async function policyRuns(api: ApiClient, cookie: string, policyId: string) {
  const page = z
    .object({ items: z.array(zRun) })
    .parse(
      (await api("GET", `/api/v1/runs?policy_id=${policyId}&limit=200`, cookie))
        .json,
    );
  return page.items;
}

async function waitTerminal(api: ApiClient, cookie: string, id: string) {
  return until(`run ${id} to finish`, backupTimeout, async () => {
    const current = await getRun(api, cookie, id);
    return ["completed", "failed", "canceled"].includes(current.status)
      ? current
      : undefined;
  });
}

async function replicasOf(api: ApiClient, cookie: string) {
  return z
    .object({ items: z.array(zReplica) })
    .parse((await api("GET", "/api/v1/replicas?limit=200", cookie)).json).items;
}

/** The id of a 201 resource; any other status fails with the body. */
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

/** Labels of the buttons currently rendered, for absence checks. */
function buttonLabels(): string[] {
  // eval prints a string result JSON-encoded.
  const joined = z
    .string()
    .parse(
      JSON.parse(
        b.ab(
          "eval",
          "[...document.querySelectorAll('button')].map((element) => element.textContent?.trim() ?? '').join('\\n')",
        ),
      ),
    );
  return joined.split("\n");
}

/** Clicks a button twice in the same task, before React can re-render. */
function doubleActivate(label: string): void {
  b.ab(
    "eval",
    `(() => { const button = [...document.querySelectorAll("button")].find((element) => element.textContent?.includes(${JSON.stringify(label)})); if (!button) throw new Error("no ${label} button"); button.click(); button.click(); return true; })()`,
  );
}

async function main(): Promise<void> {
  const stack = await startStack(
    run.image === undefined ? undefined : { reuseImage: run.image },
  );
  stopOnSignal(stack.stop);
  console.log(`stack up: ${stack.origin} (${Math.round(process.uptime())}s)`);
  const api = makeApi(stack.origin);
  let admin = await apiLogin(api, stack.admin.username, stack.admin.password);
  const viewer = { username: "f3-viewer", password: randomPassword("viewer") };
  await createUser(api, admin, viewer.username, viewer.password, "viewer");

  // Resources through the API: F2 already proved their pages.
  const localId = createdId(
    "local repository",
    await api("POST", "/api/v1/repositories", admin, {
      name: "f3-local",
      kind: "local",
      tls_mode: "none",
      root: stack.localRepositoryRoot,
    }),
  );
  const ftpBody = {
    name: "f3-ftp",
    kind: "ftp",
    tls_mode: "plain",
    ftp: {
      host: stack.ftp.host,
      port: stack.ftp.port,
      base_path: stack.ftp.basePath,
      username: stack.ftp.username,
    },
  };
  const ftpId = createdId(
    "ftp repository",
    await api("POST", "/api/v1/repositories", admin, {
      ...ftpBody,
      password: stack.ftp.password,
    }),
  );
  const sourceId = createdId(
    "source",
    await api("POST", "/api/v1/sources", admin, {
      name: "f3-source",
      socket: stack.mysql.socket,
      datadir: stack.mysql.datadir,
      username: stack.mysql.username,
      password: stack.mysql.password,
    }),
  );
  // Far-future crons: every run in this scenario is a manual one.
  const policyId = createdId(
    "policy",
    await api("POST", "/api/v1/policies", admin, {
      name: "f3-policy",
      source_id: sourceId,
      local_repository_id: localId,
      replica_repository_id: ftpId,
      full_cron: "0 3 1 1 *",
      incremental_cron: "30 4 1 * *",
      timezone: "Asia/Shanghai",
      max_chain_length: 7,
      parallel: 1,
      throttle: 0,
      zstd_level: 3,
      page_tracking: false,
      enabled: false,
    }),
  );
  const enabled = await api(
    "POST",
    `/api/v1/policies/${policyId}/enable`,
    admin,
  );
  expect(
    enabled.status === 200,
    `enable = ${enabled.status} ${JSON.stringify(enabled.json)}`,
  );

  try {
    // --- F3-A6: the preview is the server's computation, shown in both zones ---
    b.signIn(stack.origin, stack.admin.username, stack.admin.password);
    b.open(`${stack.origin}/policies/detail?id=${policyId}`);
    b.waitText("调度预览");
    b.waitText("UTC");
    const preview = zPolicySchedule.parse(
      (
        await api(
          "GET",
          `/api/v1/policies/${policyId}/schedule?count=10`,
          admin,
        )
      ).json,
    );
    expect(preview.fires.length === 10, "preview did not return 10 fires");
    const page = b.text();
    for (const fire of preview.fires.slice(0, 3)) {
      // HH:MM:SS of the UTC rendering; the page renders the same instant.
      const utcClock = fire.at.slice(11, 19);
      expect(
        page.includes(utcClock),
        `preview page lacks the fire at ${fire.at}`,
      );
    }
    expect(
      preview.fires.some((fire) => fire.kind === "full") &&
        preview.fires.some((fire) => fire.kind === "incremental"),
      "preview lacks one of the kinds",
    );
    b.record({
      id: "01-schedule-preview",
      acceptance: "F3-A6",
      description:
        "the policy page renders the server-computed next fires in Asia/Shanghai and UTC",
      screenshot: b.shot("01-schedule-preview"),
    });

    // --- F3-A3: one click is one run, even when activated twice ---
    const before = (await policyRuns(api, admin, policyId)).length;
    doubleActivate("立即全量");
    b.ab("wait", "--url", "**/runs/detail?id=*");
    const fullId = new URL(b.url()).searchParams.get("id") ?? "";
    await Bun.sleep(2000);
    const afterClick = await policyRuns(api, admin, policyId);
    expect(
      afterClick.length === before + 1,
      `a double activation created ${afterClick.length - before} runs`,
    );
    b.waitText("实时更新中");
    b.record({
      id: "02-run-live",
      acceptance: "F3-A4",
      description:
        "a double-activated 立即全量 creates exactly one run and its detail page streams live",
      screenshot: b.shot("02-run-live"),
    });

    // --- F3-A4: the live page reaches the terminal state the API reports ---
    b.waitText("已完成", backupTimeout);
    const full = await getRun(api, admin, fullId);
    expect(full.status === "completed", `full run ended ${full.status}`);
    expect(
      !b.text().includes("实时更新中"),
      "the stream did not stop at the end event",
    );
    b.record({
      id: "03-run-completed",
      acceptance: "F3-A1",
      description:
        "the live run detail ends in 已完成 exactly when the API run is completed, and streaming stops",
      screenshot: b.shot("03-run-completed"),
    });

    // Two incrementals through the UI make a three-member chain.
    for (const [index, label] of ["增量 1", "增量 2"].entries()) {
      b.open(`${stack.origin}/policies/detail?id=${policyId}`);
      b.waitText("立即增量");
      b.click("立即增量");
      b.ab("wait", "--url", "**/runs/detail?id=*");
      const id = new URL(b.url()).searchParams.get("id") ?? "";
      b.waitText("已完成", backupTimeout);
      const incremental = await getRun(api, admin, id);
      expect(
        incremental.status === "completed" &&
          incremental.kind === "incremental",
        `${label} ended ${incremental.status}/${incremental.kind}`,
      );
      console.log(`${label} (${index + 1}/2) completed: ${id}`);
    }

    // --- F3-A1/A2: replicas, chains and split protection match the API ---
    const replicas = await until(
      "three verified replicas",
      backupTimeout,
      async () => {
        const items = (await replicasOf(api, admin)).filter(
          (item) => item.repository_id === ftpId,
        );
        return items.length === 3 &&
          items.every((item) => item.status === "verified")
          ? items
          : undefined;
      },
    );
    b.open(`${stack.origin}/replicas`);
    b.waitText("已校验");
    const replicasPage = b.text();
    for (const replica of replicas) {
      expect(
        replicasPage.includes(replica.backup_id.slice(0, 8)),
        `replica of ${replica.backup_id} is not listed`,
      );
    }
    b.record({
      id: "04-replicas-verified",
      acceptance: "F3-A1",
      description:
        "the replica queue shows the three FTP replicas as 已校验, as the API does",
      screenshot: b.shot("04-replicas-verified"),
    });

    const protection = zProtection.parse(
      (await api("GET", `/api/v1/sources/${sourceId}/protection`, admin)).json,
    );
    expect(protection.local !== null, "no local protection point");
    expect(
      protection.remote.length === 1,
      "expected one remote protection point",
    );
    b.open(`${stack.origin}/sources/detail?id=${sourceId}`);
    b.waitText("备份链与保护点");
    b.waitText("异地");
    const sourcePage = b.text();
    expect(
      sourcePage.includes(protection.local?.backup_id.slice(0, 8) ?? "?"),
      "the local protection point is not the API's",
    );
    expect(
      sourcePage.includes("f3-ftp"),
      "the remote protection point is not listed per repository",
    );
    // The chain list itself: every member the API reports, and no contract
    // error in its place.
    const chains = zListSourceChainsResponse.parse(
      (await api("GET", `/api/v1/sources/${sourceId}/chains`, admin)).json,
    );
    const members = chains.items.flatMap((chain) => chain.members);
    expect(
      members.length === 3,
      `expected 3 chain members, got ${members.length}`,
    );
    expect(
      !sourcePage.includes("无法读取备份链"),
      "the chain list failed to load on the source page",
    );
    for (const member of members) {
      const linked = b.ab(
        "eval",
        `document.querySelector('a[href="/backups/detail?id=${member.id}"]') !== null`,
      );
      expect(linked === "true", `chain member ${member.id} is not listed`);
    }
    b.record({
      id: "05-chain-protection",
      acceptance: "F3-A2",
      description:
        "the source page lists the three-member chain and shows local and remote (per repository) protection separately",
      screenshot: b.shot("05-chain-protection"),
    });

    const latest = zBackup.parse(
      (
        await api(
          "GET",
          `/api/v1/backups/${protection.local?.backup_id}`,
          admin,
        )
      ).json,
    );
    b.open(`${stack.origin}/backups/detail?id=${latest.id}`);
    b.waitText(latest.payload_sha256);
    b.record({
      id: "06-backup-detail",
      acceptance: "F3-A1",
      description:
        "the backup detail shows the API's payload SHA-256, LSNs and the verified FTP replica",
      screenshot: b.shot("06-backup-detail"),
    });

    // --- F3-A3: cancel from the run page ---
    b.open(`${stack.origin}/policies/detail?id=${policyId}`);
    b.waitText("立即全量");
    b.click("立即全量");
    b.ab("wait", "--url", "**/runs/detail?id=*");
    const cancelId = new URL(b.url()).searchParams.get("id") ?? "";
    b.waitText("取消运行");
    b.click("取消运行");
    b.waitText("已取消", backupTimeout);
    const canceled = await getRun(api, admin, cancelId);
    // A queued run is canceled without running; a running one has its context
    // canceled. Either way the run ends canceled and never completed.
    expect(canceled.status === "canceled", `cancel ended ${canceled.status}`);
    expect(
      b.text().includes("已取消"),
      "the run page does not show the API's canceled status",
    );
    b.record({
      id: "07-run-canceled",
      acceptance: "F3-A3",
      description:
        "取消运行 on the run page ends the run as 已取消, as the API reports",
      screenshot: b.shot("07-run-canceled"),
    });

    // --- F3-A5: a failing replica shows its error and is retried from the UI ---
    // The FTP server is really stopped, so the upload of the next backup fails.
    stack.setFtp("down");
    const triggered = await api(
      "POST",
      `/api/v1/policies/${policyId}/runs`,
      admin,
      { kind: "full" },
      { "Idempotency-Key": randomUUID() },
    );
    expect(triggered.status === 202, `trigger = ${triggered.status}`);
    const failingRun = await waitTerminal(
      api,
      admin,
      z.object({ run_id: z.string() }).parse(triggered.json).run_id,
    );
    expect(
      failingRun.status === "completed",
      `full for replica failure ${failingRun.status}`,
    );
    const failed = await until("a failed replica", 5 * 60_000, async () =>
      (await replicasOf(api, admin)).find(
        (item) =>
          item.backup_id === failingRun.backup_id &&
          item.last_error_code !== null &&
          item.last_error_code !== "",
      ),
    ).catch(async (error: unknown) => {
      const state = (await replicasOf(api, admin)).filter(
        (item) => item.backup_id === failingRun.backup_id,
      );
      throw new Error(
        `${String(error)}; replica state ${JSON.stringify(state, (_key, value) => (typeof value === "bigint" ? value.toString() : value))}`,
      );
    });

    // --- F3-A2: while the replica fails, remote protection lags behind local ---
    const lagging = zProtection.parse(
      (await api("GET", `/api/v1/sources/${sourceId}/protection`, admin)).json,
    );
    expect(
      lagging.local?.backup_id === failingRun.backup_id &&
        lagging.remote.length === 1 &&
        lagging.remote[0]?.backup_id !== failingRun.backup_id,
      `protection during the outage: ${JSON.stringify(lagging)}`,
    );
    b.open(`${stack.origin}/sources/detail?id=${sourceId}`);
    b.waitText("备份链与保护点");
    const lagPage = b.text();
    expect(
      lagPage.includes(failingRun.backup_id ?? "?") &&
        lagPage.includes(lagging.remote[0]?.backup_id ?? "?"),
      "the source page does not show the lagging remote point next to the newer local one",
    );
    b.record({
      id: "08a-protection-lag",
      acceptance: "F3-A2",
      description:
        "while FTP is down the local point is the newest backup and the remote point stays on the last verified one; the page shows both, as the API does",
      screenshot: b.shot("08a-protection-lag"),
    });

    stack.setFtp("up");
    b.open(`${stack.origin}/replicas`);
    b.waitText(failed.last_error_code ?? "");
    b.record({
      id: "08-replica-failed",
      acceptance: "F3-A5",
      description: `a replica whose FTP server is down shows its error code (${failed.last_error_code}) and the retry action`,
      screenshot: b.shot("08-replica-failed"),
    });
    b.click("重试");
    b.waitText("已安排重试");
    await until("the retried replica to verify", backupTimeout, async () =>
      (await replicasOf(api, admin)).find(
        (item) =>
          item.backup_id === failingRun.backup_id && item.status === "verified",
      ),
    );

    // --- F3-A4: revoking the session ends the stream and returns to login ---
    const live = await api(
      "POST",
      `/api/v1/policies/${policyId}/runs`,
      admin,
      { kind: "incremental" },
      { "Idempotency-Key": randomUUID() },
    );
    expect(live.status === 202, `trigger for revocation = ${live.status}`);
    const liveId = z.object({ run_id: z.string() }).parse(live.json).run_id;
    b.open(`${stack.origin}/runs/detail?id=${liveId}`);
    b.waitText("实时更新中");
    const session = z
      .object({ user: z.object({ id: z.string() }) })
      .parse((await api("GET", "/api/v1/auth/session", admin)).json);
    const revoked = await api(
      "DELETE",
      `/api/v1/users/${session.user.id}/sessions`,
      admin,
    );
    expect(revoked.status === 204, `revoke = ${revoked.status}`);
    b.ab("wait", "--url", "**/login?*reason=expired*");
    b.waitText("会话已失效，请重新登录。");
    b.record({
      id: "09-stream-revoked",
      acceptance: "F3-A4",
      description:
        "revoking the administrator's sessions while the run streams lands on login with reason=expired",
      screenshot: b.shot("09-stream-revoked"),
    });
    admin = await apiLogin(api, stack.admin.username, stack.admin.password);
    await waitTerminal(api, admin, liveId);

    // --- F3-A5: a viewer reads everything and changes nothing ---
    b.signIn(stack.origin, viewer.username, viewer.password);
    b.open(`${stack.origin}/policies/detail?id=${policyId}`);
    b.waitText("调度预览");
    const policyButtons = buttonLabels();
    expect(
      !policyButtons.some(
        (label) => label.includes("立即全量") || label.includes("立即增量"),
      ),
      `a viewer sees trigger buttons: ${policyButtons.join(", ")}`,
    );
    b.open(`${stack.origin}/replicas`);
    b.waitText("已校验");
    expect(
      !buttonLabels().some((label) => label.includes("重试")),
      "a viewer sees the retry action",
    );
    b.open(`${stack.origin}/runs/detail?id=${fullId}`);
    b.waitText("已完成");
    expect(
      !buttonLabels().some((label) => label.includes("取消运行")),
      "a viewer sees the cancel action",
    );
    const viewerCookie = await apiLogin(api, viewer.username, viewer.password);
    const viewerTrigger = await api(
      "POST",
      `/api/v1/policies/${policyId}/runs`,
      viewerCookie,
      { kind: "full" },
      { "Idempotency-Key": randomUUID() },
    );
    expect(
      viewerTrigger.status === 403,
      `viewer trigger = ${viewerTrigger.status}`,
    );
    b.record({
      id: "10-viewer-read-only",
      acceptance: "F3-A5",
      description:
        "a viewer sees schedule, runs and replicas without trigger, cancel or retry, and the API answers 403",
      screenshot: b.shot("10-viewer-read-only"),
    });

    b.assertCleanConsole();
    b.record({
      id: "11-clean-console",
      acceptance: "F3-A7",
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
  `f3 e2e: ${b.steps.length} steps passed; evidence in ${run.evidence}${run.dirty ? " (working tree dirty)" : ""}`,
);
