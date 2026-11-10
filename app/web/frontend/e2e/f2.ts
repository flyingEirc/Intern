// F2 (#56) acceptance through the real UI: repositories, sources and policies
// against the real-component stack (e2e/stack.ts: Compose deployment, MySQL
// 8.4.11 + PXB 8.4.0-7, plain and explicit-TLS Pure-FTPd 1.0.54) driven by
// agent-browser. Every assertion reads observable state: rendered text, the
// browser URL, and the same server's API.
//
//   bun e2e/f2.ts [--evidence DIR] [--image TAG] [--headed]
//
// Screenshots and manifest.json go to DIR (default <repo>/.cache/e2e-f2/<commit>).
// On an arm64 Mac the stack runs under amd64 emulation: development evidence
// only; binding acceptance is native linux/amd64.

import { z } from "zod";
import {
  zErrorResponse,
  zGetPolicyResponse,
  zPreflightResult,
  zSource,
} from "../lib/api/generated/zod.gen";
import {
  checkLabel,
  preflightCheckLabel,
  preflightUnknownCheckLabel,
} from "../lib/resources/preflight";
import {
  apiLogin,
  Browser,
  createUser,
  expect,
  makeApi,
  randomPassword,
  runContext,
  stopOnSignal,
} from "./harness";
import { startStack } from "./stack";

const run = runContext("f2");
const b = new Browser(`intern-f2-e2e-${process.pid}`, run.evidence, run.headed);

async function main(): Promise<void> {
  const stack = await startStack(
    run.image === undefined ? undefined : { reuseImage: run.image },
  );
  stopOnSignal(stack.stop);
  console.log(`stack up: ${stack.origin} (${Math.round(process.uptime())}s)`);
  const api = makeApi(stack.origin);
  const adminCookie = await apiLogin(
    api,
    stack.admin.username,
    stack.admin.password,
  );
  const operator = {
    username: "f2-operator",
    password: randomPassword("operator"),
  };
  const viewer = { username: "f2-viewer", password: randomPassword("viewer") };
  await createUser(
    api,
    adminCookie,
    operator.username,
    operator.password,
    "operator",
  );
  await createUser(
    api,
    adminCookie,
    viewer.username,
    viewer.password,
    "viewer",
  );

  try {
    // --- F2-A2: plain FTP is an explicit choice and is flagged everywhere ---
    b.open(`${stack.origin}/login`);
    b.ab("wait", "--text", "使用 Intern 账号登录控制台");
    b.fill("用户名", stack.admin.username);
    b.fill("密码", stack.admin.password);
    b.click("登录");
    b.ab("wait", "--text", "数据保护概览");

    b.open(`${stack.origin}/repositories/new`);
    b.ab("wait", "--text", "新建仓库");
    b.fill("名称", "f2-plain-ftp");
    b.click("FTP / FTPS", "radio");
    b.click("创建仓库");
    b.ab("wait", "--text", "请选择 FTP 传输方式");
    const tlsRequests = await api("GET", "/api/v1/repositories", adminCookie);
    expect(
      !JSON.stringify(tlsRequests.json).includes("f2-plain-ftp"),
      "a repository without an explicit TLS choice reached the server",
    );
    b.click("明文 FTP", "radio");
    b.ab("wait", "--text", "凭据与数据明文传输");
    b.fill("主机", stack.ftp.host);
    b.fill("端口", String(stack.ftp.port));
    b.fill("基础路径", stack.ftp.basePath);
    b.fill("用户名", stack.ftp.username);
    b.fill("密码", stack.ftp.password);
    b.record({
      id: "01-plain-ftp-form",
      acceptance: "F2-A2",
      description:
        "FTP repository cannot be saved without an explicit TLS choice, and choosing plain FTP shows the permanent warning",
      screenshot: b.shot("01-plain-ftp-form"),
    });
    b.click("创建仓库");
    b.ab("wait", "--text", "仓库已创建");
    const repositories = z
      .object({
        items: z.array(
          z.object({
            id: z.string(),
            name: z.string(),
            kind: z.string(),
            tls_mode: z.string(),
            has_secret: z.boolean(),
            plaintext_credentials: z.boolean(),
          }),
        ),
      })
      .parse((await api("GET", "/api/v1/repositories", adminCookie)).json);
    const plainRepository = repositories.items.find(
      (item) => item.name === "f2-plain-ftp",
    );
    if (plainRepository === undefined) {
      throw new Error("f2 e2e: plain FTP repository was not created");
    }
    expect(
      plainRepository.tls_mode === "plain" &&
        plainRepository.plaintext_credentials === true &&
        plainRepository.has_secret === true,
      `plain FTP repository stored as ${JSON.stringify(plainRepository)}`,
    );
    expect(
      !JSON.stringify(repositories).includes(stack.ftp.password),
      "the repository API echoed the stored password",
    );
    b.open(`${stack.origin}/repositories/detail?id=${plainRepository.id}`);
    b.ab("wait", "--text", "写入性探测");
    expect(
      b.text().includes("凭据与数据明文传输"),
      "detail page lacks the plaintext warning",
    );
    b.click("运行探测");
    b.ab("wait", "--text", "探测通过");
    b.record({
      id: "02-plain-ftp-detail",
      acceptance: "F2-A2",
      description:
        "plain FTP repository detail keeps the plaintext warning, never shows the password, and probes successfully",
      screenshot: b.shot("02-plain-ftp-detail"),
    });

    // --- F2-A1: write-only secrets and a real preflight ---
    const local = await api("POST", "/api/v1/repositories", adminCookie, {
      name: "f2-local",
      kind: "local",
      tls_mode: "none",
      root: stack.localRepositoryRoot,
    });
    expect(local.status === 201, `create local repository = ${local.status}`);

    b.open(`${stack.origin}/sources/new`);
    b.ab("wait", "--text", "新建数据源");
    b.fill("名称", "f2-source");
    b.fill("连接路径（Unix socket）", stack.mysql.socket);
    b.fill("数据目录", stack.mysql.datadir);
    b.fill("用户名", stack.mysql.username);
    b.fill("密码", stack.mysql.password);
    b.click("创建数据源");
    b.ab("wait", "--text", "数据源已创建");
    const sources = z
      .object({ items: z.array(zSource) })
      .parse((await api("GET", "/api/v1/sources", adminCookie)).json);
    const source = sources.items.find((item) => item.name === "f2-source");
    if (source === undefined) {
      throw new Error("f2 e2e: source was not created");
    }
    expect(source?.has_secret === true, "source was not stored with a secret");
    expect(
      !JSON.stringify(sources).includes(stack.mysql.password),
      "the source API echoed the stored password",
    );

    b.open(`${stack.origin}/sources/detail?id=${source.id}`);
    b.ab("wait", "--text", "运行预检");
    b.click("运行预检");
    b.ab("wait", "--text", "本次预检结果");
    b.ab("wait", "--text", "检查项");
    const preflightText = b.text();
    for (const code of [
      "identity",
      "compatibility",
      "volume",
      "mount",
      "privileges",
      "capacity",
    ]) {
      const label = checkLabel(code);
      expect(
        label !== preflightUnknownCheckLabel && preflightText.includes(label),
        `preflight output lacks the ${code} check (${label})`,
      );
    }
    expect(
      !preflightText.includes("未通过"),
      "a passing preflight rendered the failure badge",
    );
    b.record({
      id: "03-preflight-real",
      acceptance: "F2-A3",
      description:
        "preflight against the real MySQL 8.4.11 + PXB pair lists every check and passes",
      screenshot: b.shot("03-preflight-real"),
    });

    b.open(`${stack.origin}/sources/detail?id=${source.id}`);
    b.ab("wait", "--text", "编辑");
    b.click("编辑", "link");
    b.ab("wait", "--text", "保存修改");
    const passwordValue = b.ab("get", "attr", "#source-password", "value");
    expect(
      passwordValue === "",
      `the edit form pre-filled the password (${passwordValue})`,
    );
    b.fill("名称", "f2-source-renamed");
    b.click("保存修改");
    b.ab("wait", "--text", "数据源已更新");
    const afterRename = z
      .object({ items: z.array(zSource) })
      .parse((await api("GET", "/api/v1/sources", adminCookie)).json);
    const renamed = afterRename.items.find((item) => item.id === source.id);
    expect(
      renamed !== undefined &&
        renamed.name === "f2-source-renamed" &&
        renamed.has_secret,
      "renaming without a password dropped or replaced the stored secret",
    );
    const rePreflight = await api(
      "POST",
      `/api/v1/sources/${source.id}/preflight`,
      adminCookie,
      {
        local_repository_id:
          (local.json as { repository?: { id: string }; id?: string })
            .repository?.id ?? (local.json as { id: string }).id,
      },
    );
    expect(
      rePreflight.status === 200 &&
        (rePreflight.json as { ok?: boolean }).ok === true,
      `preflight after the password-preserving update = ${rePreflight.status} ${JSON.stringify(rePreflight.json)}`,
    );
    b.record({
      id: "04-write-only-secret",
      acceptance: "F2-A1",
      description:
        "the edit form never renders the stored password, and saving without it keeps the secret usable (preflight still passes)",
      screenshot: b.shot("04-write-only-secret"),
    });

    // --- F2-A3: a failing preflight is shown and writes nothing ---
    const badSource = await api("POST", "/api/v1/sources", adminCookie, {
      name: "f2-bad-source",
      socket: stack.mysql.socket,
      datadir: stack.mysql.datadir,
      username: stack.mysql.username,
      password: "wrong-password-for-f2",
    });
    expect(badSource.status === 201, `create bad source = ${badSource.status}`);
    const badSourceId = (badSource.json as { id: string }).id;
    const failedPreflight = await api(
      "POST",
      `/api/v1/sources/${badSourceId}/preflight`,
      adminCookie,
      {
        local_repository_id: (local.json as { id: string }).id,
      },
    );
    expect(
      failedPreflight.status === 200,
      `bad preflight status ${failedPreflight.status}`,
    );
    const failedResult = zPreflightResult.parse(failedPreflight.json);
    expect(!failedResult.ok, "a wrong password produced a passing preflight");
    b.open(`${stack.origin}/sources/detail?id=${badSourceId}`);
    b.ab("wait", "--text", "运行预检");
    b.click("运行预检");
    b.ab("wait", "--text", "本次预检结果");
    b.ab("wait", "--text", "未通过");
    // The main view names the failing check in business words; machine codes
    // stay in the diagnostics fold. A code the table lacks keeps the generic
    // name, so there the raw code must still be reachable after expanding it.
    const failure = failedResult.error;
    expect(
      failure !== undefined,
      `the backend reported no failing check: ${JSON.stringify(failedResult)}`,
    );
    const failureText = b.text();
    const failingCheck = failure?.check ?? "";
    if (preflightCheckLabel[failingCheck] !== undefined) {
      const label = checkLabel(failingCheck);
      expect(
        failureText.includes(label),
        `the page does not show the failing check ${failingCheck} as ${label}; backend ${JSON.stringify(failedResult)}; page: ${failureText.slice(0, 1200)}`,
      );
    } else {
      b.click("预检诊断");
      const expanded = b.text();
      expect(
        failingCheck !== "" && expanded.includes(failingCheck),
        `the page does not show the unknown failing check ${failingCheck}; backend ${JSON.stringify(failedResult)}; page: ${expanded.slice(0, 1200)}`,
      );
      const failingCode = failure?.code ?? "";
      expect(
        failingCode !== "" && expanded.includes(failingCode),
        `the page does not show the unknown failure code ${failingCode}; backend ${JSON.stringify(failedResult)}; page: ${expanded.slice(0, 1200)}`,
      );
    }
    b.record({
      id: "05-preflight-failed",
      acceptance: "F2-A3",
      description:
        "a source with a wrong password fails preflight and the page shows the structured failure",
      screenshot: b.shot("05-preflight-failed"),
    });

    // --- F2-A4: enable and update failures keep the stored policy state ---
    const localId = (local.json as { id: string }).id;
    const policyBody = {
      name: "f2-policy-bad",
      source_id: badSourceId,
      local_repository_id: localId,
      replica_repository_id: null,
      full_cron: "0 2 * * *",
      incremental_cron: "0 * * * *",
      timezone: "Asia/Shanghai",
      enabled: false,
    };
    const badPolicy = await api(
      "POST",
      "/api/v1/policies",
      adminCookie,
      policyBody,
    );
    expect(badPolicy.status === 201, `create policy = ${badPolicy.status}`);
    const badPolicyId = (badPolicy.json as { id: string }).id;
    b.open(`${stack.origin}/policies/detail?id=${badPolicyId}`);
    b.ab("wait", "--text", "未启用");
    b.click("启用");
    b.ab("wait", "--text", "启用失败");
    const stillDisabled = await api(
      "GET",
      `/api/v1/policies/${badPolicyId}`,
      adminCookie,
    );
    expect(
      (stillDisabled.json as { enabled?: boolean }).enabled === false,
      "a policy whose enable preflight failed became enabled",
    );
    b.record({
      id: "06-enable-refused",
      acceptance: "F2-A4",
      description:
        "enabling a policy whose source preflight fails shows the outcome and leaves it disabled",
      screenshot: b.shot("06-enable-refused"),
    });

    const goodPolicy = await api("POST", "/api/v1/policies", adminCookie, {
      ...policyBody,
      name: "f2-policy-good",
      source_id: source.id,
    });
    expect(
      goodPolicy.status === 201,
      `create good policy = ${goodPolicy.status}`,
    );
    const goodPolicyId = (goodPolicy.json as { id: string }).id;
    const enabled = await api(
      "POST",
      `/api/v1/policies/${goodPolicyId}/enable`,
      adminCookie,
    );
    expect(enabled.status === 200, `enable good policy = ${enabled.status}`);
    b.open(`${stack.origin}/policies/detail?id=${goodPolicyId}`);
    b.ab("wait", "--text", "已启用");
    b.click("编辑", "link");
    b.ab("wait", "--text", "保存修改");
    b.ab("find", "role", "combobox", "click", "--name", "数据源");
    b.ab("wait", "--text", "f2-bad-source");
    b.ab("find", "role", "option", "click", "--name", "f2-bad-source");
    b.click("自定义全量");
    b.fill("全量安排", "5 3 * * *");
    b.click("保存修改");
    b.ab("wait", "--text", "保存被预检拒绝");
    const afterFailedUpdate = await api(
      "GET",
      `/api/v1/policies/${goodPolicyId}`,
      adminCookie,
    );
    const updated = afterFailedUpdate.json as {
      enabled?: boolean;
      full_cron?: string;
      source_id?: string;
    };
    expect(
      updated.enabled === true &&
        updated.full_cron === "0 2 * * *" &&
        updated.source_id === source.id,
      `a rejected update changed the policy: ${JSON.stringify(updated)}`,
    );
    b.record({
      id: "07-update-refused",
      acceptance: "F2-A4",
      description:
        "a rejected update of an enabled policy keeps it enabled with its original source and cron",
      screenshot: b.shot("07-update-refused"),
    });

    // B2: a name-only edit through the form must not touch the five tuning columns.
    const beforePolicyRename = zGetPolicyResponse.parse(
      (await api("GET", `/api/v1/policies/${goodPolicyId}`, adminCookie)).json,
    );
    b.open(`${stack.origin}/policies/new?id=${goodPolicyId}`);
    b.ab("wait", "--text", "\u4fdd\u5b58\u4fee\u6539");
    b.fill("\u540d\u79f0", "f2-policy-renamed");
    b.click("\u4fdd\u5b58\u4fee\u6539");
    b.ab("wait", "--text", "\u7b56\u7565\u5df2\u66f4\u65b0");
    const afterPolicyRename = zGetPolicyResponse.parse(
      (await api("GET", `/api/v1/policies/${goodPolicyId}`, adminCookie)).json,
    );
    expect(
      afterPolicyRename.name === "f2-policy-renamed" &&
        afterPolicyRename.max_chain_length ===
          beforePolicyRename.max_chain_length &&
        afterPolicyRename.parallel === beforePolicyRename.parallel &&
        afterPolicyRename.throttle === beforePolicyRename.throttle &&
        afterPolicyRename.zstd_level === beforePolicyRename.zstd_level &&
        afterPolicyRename.page_tracking === beforePolicyRename.page_tracking &&
        afterPolicyRename.full_cron === beforePolicyRename.full_cron &&
        afterPolicyRename.incremental_cron ===
          beforePolicyRename.incremental_cron &&
        afterPolicyRename.source_id === beforePolicyRename.source_id &&
        afterPolicyRename.local_repository_id ===
          beforePolicyRename.local_repository_id &&
        afterPolicyRename.enabled === beforePolicyRename.enabled,
      `a name-only edit changed tuning: ${JSON.stringify(afterPolicyRename)}`,
    );
    b.record({
      id: "07b-name-only-keeps-tuning",
      acceptance: "B2",
      description:
        "renaming an enabled policy through the form keeps the five server-owned tuning values, both schedules and the source",
      screenshot: b.shot("07b-name-only-keeps-tuning"),
    });

    // --- F2-A5: role-based visibility ---
    b.signOut();
    b.fill("用户名", viewer.username);
    b.fill("密码", viewer.password);
    b.click("登录");
    b.ab("wait", "--text", "数据保护概览");
    b.open(`${stack.origin}/repositories`);
    b.ab("wait", "--text", "备份仓库");
    expect(
      !b.text().includes("新建仓库"),
      "a viewer sees the create-repository action",
    );
    b.open(`${stack.origin}/repositories/new`);
    b.ab("wait", "--text", "没有权限");
    const viewerWrite = await api(
      "POST",
      "/api/v1/repositories",
      await apiLogin(api, viewer.username, viewer.password),
      {
        name: "viewer-should-fail",
        kind: "local",
        tls_mode: "none",
        root: stack.localRepositoryRoot,
      },
    );
    expect(viewerWrite.status === 403, `viewer write = ${viewerWrite.status}`);
    b.record({
      id: "08-viewer-read-only",
      acceptance: "F2-A5",
      description:
        "a viewer has no write actions, the create page refuses, and the API answers 403",
      screenshot: b.shot("08-viewer-read-only"),
    });

    // The login page sends a signed-in visitor straight to the console, so the
    // viewer session is closed before the next sign-in.
    b.signOut();
    b.ab("wait", "--text", "使用 Intern 账号登录控制台");
    b.fill("用户名", operator.username);
    b.fill("密码", operator.password);
    b.click("登录");
    b.ab("wait", "--text", "数据保护概览");
    b.open(`${stack.origin}/repositories/new`);
    b.ab("wait", "--text", "没有权限");
    b.open(`${stack.origin}/sources/new`);
    b.ab("wait", "--text", "新建数据源");
    const operatorRepository = await api(
      "POST",
      "/api/v1/repositories",
      await apiLogin(api, operator.username, operator.password),
      {
        name: "operator-should-fail",
        kind: "local",
        tls_mode: "none",
        root: stack.localRepositoryRoot,
      },
    );
    expect(
      operatorRepository.status === 403,
      `operator repository write = ${operatorRepository.status}`,
    );
    b.record({
      id: "09-operator-scope",
      acceptance: "F2-A5",
      description:
        "an operator can open the source form but not manage repositories",
      screenshot: b.shot("09-operator-scope"),
    });

    // --- F2-A6: an unconfigured deployment explains itself ---
    if (stack.noKek === undefined) {
      throw new Error(
        "f2 e2e: the stack did not provide a KEK-less deployment",
      );
    }
    const noKekApi = makeApi(stack.noKek.origin);
    await apiLogin(
      noKekApi,
      stack.noKek.admin.username,
      stack.noKek.admin.password,
    );
    b.open(`${stack.noKek.origin}/login`);
    b.ab("wait", "--text", "使用 Intern 账号登录控制台");
    b.fill("用户名", stack.noKek.admin.username);
    b.fill("密码", stack.noKek.admin.password);
    b.click("登录");
    b.ab("wait", "--text", "数据保护概览");
    b.open(`${stack.noKek.origin}/sources/new`);
    b.ab("wait", "--text", "新建数据源");
    b.fill("名称", "no-kek-source");
    b.fill("连接路径（Unix socket）", stack.mysql.socket);
    b.fill("数据目录", stack.mysql.datadir);
    b.fill("用户名", stack.mysql.username);
    b.fill("密码", stack.mysql.password);
    b.click("创建数据源");
    b.ab("wait", "--text", "当前部署缺少完成此操作所需的配置");
    const noKekResponse = await api("POST", "/api/v1/sources", undefined, {});
    expect(
      noKekResponse.status === 401,
      "the KEK-less API answered an unauthenticated write",
    );
    b.record({
      id: "10-no-kek",
      acceptance: "F2-A6",
      description:
        "without a KEK the create form reports the missing deployment configuration instead of an unhandled error",
      screenshot: b.shot("10-no-kek"),
    });

    // The API contract of the failure the page rendered above.
    const noKekCookie = await apiLogin(
      noKekApi,
      stack.noKek.admin.username,
      stack.noKek.admin.password,
    );
    const unavailable = await noKekApi("POST", "/api/v1/sources", noKekCookie, {
      name: "no-kek-source",
      socket: stack.mysql.socket,
      datadir: stack.mysql.datadir,
      username: stack.mysql.username,
      password: stack.mysql.password,
    });
    const error = zErrorResponse.parse(unavailable.json);
    expect(
      unavailable.status === 503 && error.error.code === "unavailable",
      `KEK-less source create = ${unavailable.status} ${error.error.code}`,
    );

    b.assertCleanConsole();
    b.record({
      id: "11-clean-console",
      acceptance: "F2-A2",
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
  `f2 e2e: ${b.steps.length} steps passed; evidence in ${run.evidence}${run.dirty ? " (working tree dirty)" : ""}`,
);
