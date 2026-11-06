// F5 (#59) acceptance of user management and the audit log, driven by
// agent-browser against a real bin/intern-web built with the webui tag
// (`mise run build`). The suite needs only that binary and SQLite.
//
//   bun e2e/f5.ts [--evidence DIR] [--headed]
//
// Screenshots and manifest.json go to DIR (default
// <repo>/.cache/e2e-f5/<commit>). Test passwords are random per run and are
// never written to the evidence or asserted through page text.

import { z } from "zod";
import {
  type zAuditLog,
  zListAuditLogsResponse,
  zUser,
} from "../lib/api/generated/zod.gen";
import {
  type ApiClient,
  apiLogin,
  Browser,
  expect,
  makeApi,
  randomPassword,
  runContext,
  startNative,
  stopOnSignal,
  until,
} from "./harness";

const run = runContext("f5");
const adminBrowser = new Browser(
  `intern-f5-admin-${process.pid}`,
  run.evidence,
  run.headed,
);
const subjectBrowser = new Browser(
  `intern-f5-subject-${process.pid}`,
  run.evidence,
  run.headed,
);

const admin = { username: "f5-admin", password: randomPassword("admin") };
const operator = {
  username: "f5-operator",
  password: randomPassword("operator"),
};
const viewer = { username: "f5-viewer", password: randomPassword("viewer") };
const secondAdmin = {
  username: "f5-admin-2",
  password: randomPassword("admin2"),
};
const resetPassword = randomPassword("reset");

const operatorPermissions = [
  "session:self",
  "sources:manage",
  "policies:manage",
  "resources:read",
  "runs:read",
  "runs:trigger",
  "runs:cancel",
  "replicas:retry",
  "restores:read",
  "restores:plan",
];
const viewerPermissions = [
  "session:self",
  "resources:read",
  "runs:read",
  "restores:read",
];

const zUserList = z.object({ items: z.array(zUser) });

function linkLabels(browser: Browser): string[] {
  const joined = z
    .string()
    .parse(
      JSON.parse(
        browser.ab(
          "eval",
          "[...document.querySelectorAll('a')].map((element) => element.textContent?.trim() ?? '').join('\\n')",
        ),
      ),
    );
  return joined.split("\n").filter((label) => label !== "");
}

function pageHtml(browser: Browser): string {
  return z
    .string()
    .parse(
      JSON.parse(browser.ab("eval", "document.documentElement.outerHTML")),
    );
}

/** Every audit row, walked with the contract's `before` cursor. */
async function allAudit(
  api: ApiClient,
  cookie: string,
): Promise<Array<z.infer<typeof zAuditLog>>> {
  const rows: Array<z.infer<typeof zAuditLog>> = [];
  let before: string | undefined;
  for (;;) {
    const page = zListAuditLogsResponse.parse(
      (
        await api(
          "GET",
          `/api/v1/audit-logs?limit=50${before === undefined ? "" : `&before=${before}`}`,
          cookie,
        )
      ).json,
    );
    rows.push(...page.items);
    if (page.next_before === null) {
      return rows;
    }
    before = String(page.next_before);
  }
}

function choose(browser: Browser, label: string, option: string): void {
  browser.ab("find", "role", "combobox", "click", "--name", label);
  browser.ab(
    "wait",
    "--fn",
    `[...document.querySelectorAll('[role=option]')].some((item) => (item.textContent || '').trim() === ${JSON.stringify(option)})`,
  );
  browser.ab(
    "eval",
    `(() => { const item = [...document.querySelectorAll('[role=option]')].find((option) => (option.textContent || '').trim() === ${JSON.stringify(option)}); if (!item) throw new Error('option missing'); item.click(); return true; })()`,
  );
}

/** Client navigation so the cached session makes the 401 a real expiry. */
function expireOpenSession(browser: Browser, link: string): void {
  browser.click(link, "link");
  browser.ab("wait", "--url", "**/login**");
  browser.waitText("会话已失效，请重新登录。");
}

function browserSessionCookie(browser: Browser): string {
  const cookies = z
    .object({
      data: z.object({
        cookies: z.array(z.object({ name: z.string(), value: z.string() })),
      }),
    })
    .parse(JSON.parse(browser.ab("cookies", "--json")));
  const session = cookies.data.cookies.find(
    (cookie) => cookie.name === "intern_session",
  );
  expect(session !== undefined, "browser holds no session cookie");
  return `intern_session=${session?.value}`;
}

function expectPermissions(
  browser: Browser,
  permissions: readonly string[],
  absent: readonly string[],
): void {
  const text = browser.text();
  for (const permission of permissions) {
    expect(text.includes(permission), `overview missing ${permission}`);
  }
  for (const permission of absent) {
    expect(!text.includes(permission), `overview shows ${permission}`);
  }
}

await main().catch((error: unknown) => {
  adminBrowser.close();
  subjectBrowser.close();
  throw error;
});

async function main(): Promise<void> {
  const server = await startNative({ admin });
  stopOnSignal(server.stop);
  console.log(`intern-web up: ${server.origin}`);
  const api = makeApi(server.origin);
  let adminCookie = await apiLogin(api, admin.username, admin.password);

  try {
    adminBrowser.ab("set", "viewport", "1440", "900");
    subjectBrowser.ab("set", "viewport", "1280", "800");
    await createAndSignIn(server.origin, api, adminCookie);
    await refuseOtherRoles(server.origin, api);
    adminCookie = await changeAndRevoke(server.origin, api, adminCookie);
    await guardLastAdmin(server.origin, api, adminCookie);
    await checkAudit(server.origin, api, adminCookie);
    adminBrowser.assertCleanConsole();
    subjectBrowser.assertCleanConsole();
    adminBrowser.record({
      id: "17-clean-console",
      acceptance: "F5-A6",
      description:
        "neither browser session reported a page error or CSP violation",
    });
  } finally {
    adminBrowser.close();
    subjectBrowser.close();
    await server.stop();
  }
}

async function createAndSignIn(
  origin: string,
  api: ApiClient,
  adminCookie: string,
): Promise<void> {
  adminBrowser.signIn(origin, admin.username, admin.password);
  adminBrowser.ab("find", "role", "link", "click", "--name", "用户");
  adminBrowser.waitText("新建用户");
  adminBrowser.record({
    id: "01-user-list",
    acceptance: "F5-A1",
    description:
      "the administrator sees the user list, including the bootstrap admin",
    screenshot: adminBrowser.shot("01-user-list"),
  });

  adminBrowser.ab(
    "eval",
    "document.querySelector('a[href$=\"/users/new\"]').click()",
  );
  adminBrowser.waitText("初始密码");
  adminBrowser.fill("用户名", operator.username);
  adminBrowser.fill("初始密码", operator.password);
  choose(adminBrowser, "角色", "操作员");
  adminBrowser.record({
    id: "02-create-operator",
    acceptance: "F5-A1",
    description: "the create form before submit; the password field is masked",
    screenshot: adminBrowser.shot("02-create-operator"),
  });
  adminBrowser.click("创建用户");
  adminBrowser.waitText("保存会吊销会话");
  expect(
    adminBrowser.url().includes("/users/detail?id="),
    "creating a user opens its detail",
  );
  expect(
    !adminBrowser.text().includes(operator.password),
    "the created user's password is shown after creation",
  );

  adminBrowser.ab("find", "role", "link", "click", "--name", "用户");
  adminBrowser.waitText(operator.username);
  adminBrowser.waitText("新建用户");
  adminBrowser.click("新建用户");
  adminBrowser.waitText("初始密码");
  adminBrowser.fill("用户名", viewer.username);
  adminBrowser.fill("初始密码", viewer.password);
  choose(adminBrowser, "角色", "只读");
  adminBrowser.click("创建用户");
  adminBrowser.waitText("保存会吊销会话");

  const listed = zUserList.parse(
    (await api("GET", "/api/v1/users", adminCookie)).json,
  );
  adminBrowser.ab("find", "role", "link", "click", "--name", "用户");
  adminBrowser.waitText(viewer.username);
  const listText = adminBrowser.text();
  for (const account of [admin, operator, viewer]) {
    const row = listed.items.find((item) => item.username === account.username);
    expect(row !== undefined, `${account.username} missing from the API list`);
    expect(
      listText.includes(account.username),
      `${account.username} missing from the rendered list`,
    );
  }
  adminBrowser.record({
    id: "03-users-listed",
    acceptance: "F5-A1",
    description:
      "operator and viewer appear in the list with the same usernames the API returns",
    screenshot: adminBrowser.shot("03-users-listed"),
  });
}

async function refuseOtherRoles(origin: string, api: ApiClient): Promise<void> {
  subjectBrowser.signIn(origin, viewer.username, viewer.password);
  expectPermissions(subjectBrowser, viewerPermissions, [
    "users:manage",
    "runs:trigger",
  ]);
  const viewerLinks = linkLabels(subjectBrowser);
  expect(!viewerLinks.includes("用户"), "viewer sidebar shows 用户");
  expect(!viewerLinks.includes("审计日志"), "viewer sidebar shows 审计日志");
  subjectBrowser.open(`${origin}/audit`);
  subjectBrowser.waitText("没有权限");
  subjectBrowser.record({
    id: "06-viewer-no-audit",
    acceptance: "F5-A5",
    description:
      "a viewer opening /audit directly sees the no-permission state",
    screenshot: subjectBrowser.shot("06-viewer-no-audit"),
  });
  const viewerCookie = browserSessionCookie(subjectBrowser);
  expect(
    (await api("GET", "/api/v1/users", viewerCookie)).status === 403,
    "viewer users is not 403",
  );
  expect(
    (await api("GET", "/api/v1/audit-logs", viewerCookie)).status === 403,
    "viewer audit is not 403",
  );

  subjectBrowser.signOut();
  subjectBrowser.fill("用户名", operator.username);
  subjectBrowser.fill("密码", operator.password);
  subjectBrowser.click("登录");
  subjectBrowser.waitText("有效权限");
  expectPermissions(subjectBrowser, operatorPermissions, ["users:manage"]);
  subjectBrowser.record({
    id: "04-operator-signed-in",
    acceptance: "F5-A1",
    description:
      "the new operator signs in with the initial password and gets operator permissions",
    screenshot: subjectBrowser.shot("04-operator-overview"),
  });
  const links = linkLabels(subjectBrowser);
  expect(!links.includes("用户"), "operator sidebar shows 用户");
  expect(!links.includes("审计日志"), "operator sidebar shows 审计日志");
  subjectBrowser.open(`${origin}/users`);
  subjectBrowser.waitText("没有权限");
  subjectBrowser.record({
    id: "05-operator-no-users",
    acceptance: "F5-A5",
    description:
      "an operator opening /users directly sees the no-permission state",
    screenshot: subjectBrowser.shot("05-operator-no-users"),
  });
  const operatorCookie = browserSessionCookie(subjectBrowser);
  expect(
    (await api("GET", "/api/v1/users", operatorCookie)).status === 403,
    "operator users is not 403",
  );
  expect(
    (await api("GET", "/api/v1/audit-logs", operatorCookie)).status === 403,
    "operator audit is not 403",
  );
}

async function changeAndRevoke(
  origin: string,
  api: ApiClient,
  adminCookie: string,
): Promise<string> {
  const listed = zUserList.parse(
    (await api("GET", "/api/v1/users", adminCookie)).json,
  );
  const operatorRow = listed.items.find(
    (item) => item.username === operator.username,
  );
  expect(operatorRow !== undefined, "operator row missing");
  const operatorId = operatorRow?.id ?? "";

  adminBrowser.open(`${origin}/users/detail?id=${operatorId}`);
  adminBrowser.waitText("保存会吊销会话");
  choose(adminBrowser, "角色", "只读");
  adminBrowser.record({
    id: "07-role-change",
    acceptance: "F5-A2",
    description: "the operator detail before the role change is saved",
    screenshot: adminBrowser.shot("07-role-change"),
  });
  adminBrowser.click("保存修改");
  adminBrowser.waitText("用户已更新");
  const demoted = zUser.parse(
    (await api("GET", `/api/v1/users/${operatorId}`, adminCookie)).json,
  );
  expect(demoted.role === "viewer", `role after change = ${demoted.role}`);

  expireOpenSession(subjectBrowser, "备份仓库");
  subjectBrowser.record({
    id: "08-revoked-by-role",
    acceptance: "F5-A2",
    description:
      "the operator's open session is 401 after the role change and lands on login",
    screenshot: subjectBrowser.shot("08-revoked-by-role"),
  });

  // The role change consumed the operator's open session. Sign back in with the
  // same password, now as a viewer, then exercise disable, reset and revoke.
  subjectBrowser.fill("用户名", operator.username);
  subjectBrowser.fill("密码", operator.password);
  subjectBrowser.click("登录");
  subjectBrowser.ab("wait", "--url", "**/repositories**");
  subjectBrowser.open(`${origin}/`);
  subjectBrowser.waitText("有效权限");
  expect(
    !subjectBrowser.text().includes("runs:trigger"),
    "the demoted user still shows runs:trigger",
  );

  adminBrowser.open(`${origin}/users/detail?id=${operatorId}`);
  adminBrowser.waitText("保存会吊销会话");
  choose(adminBrowser, "启用状态", "已禁用");
  adminBrowser.click("保存修改");
  adminBrowser.waitText("用户已更新");
  expireOpenSession(subjectBrowser, "备份仓库");
  subjectBrowser.fill("用户名", operator.username);
  subjectBrowser.fill("密码", operator.password);
  subjectBrowser.click("登录");
  subjectBrowser.waitText("用户名或密码错误。");
  adminBrowser.record({
    id: "09-disabled",
    acceptance: "F5-A2",
    description:
      "disabling the user ends the open session and a login is refused",
    screenshot: adminBrowser.shot("09-disabled"),
  });

  choose(adminBrowser, "启用状态", "已启用");
  adminBrowser.fill("重置密码", resetPassword);
  adminBrowser.click("保存修改");
  // The 用户已更新 toast of the disable step may still be on screen, so wait
  // for the save itself: the field only empties once the reset succeeded.
  adminBrowser.ab(
    "wait",
    "--fn",
    "document.querySelector('#user-password')?.value === ''",
  );
  const passwordValue = z
    .string()
    .parse(
      JSON.parse(
        adminBrowser.ab(
          "eval",
          "document.querySelector('#user-password')?.value ?? ''",
        ),
      ),
    );
  expect(
    passwordValue === "" && !adminBrowser.text().includes(resetPassword),
    `the reset password remains in the page: ${passwordValue}`,
  );
  const oldLogin = await api("POST", "/api/v1/auth/login", undefined, {
    username: operator.username,
    password: operator.password,
  });
  expect(
    oldLogin.status === 401,
    `old password after reset = ${oldLogin.status}`,
  );
  const stillViewer = zUser.parse(
    (await api("GET", `/api/v1/users/${operatorId}`, adminCookie)).json,
  );
  expect(stillViewer.role === "viewer", "password reset changed the role");
  subjectBrowser.fill("用户名", operator.username);
  subjectBrowser.fill("密码", resetPassword);
  subjectBrowser.click("登录");
  subjectBrowser.ab("wait", "--url", "**/repositories**");
  adminBrowser.open(`${origin}/users/detail?id=${operatorId}`);
  adminBrowser.waitText("吊销全部会话");
  adminBrowser.click("吊销全部会话");
  adminBrowser.waitText("确认吊销");
  adminBrowser.record({
    id: "10-revoke-confirm",
    acceptance: "F5-A2",
    description: "revoking sessions asks for confirmation before it happens",
    screenshot: adminBrowser.shot("10-revoke-confirm"),
  });
  adminBrowser.click("确认吊销");
  adminBrowser.waitText("会话已吊销");
  expireOpenSession(subjectBrowser, "MySQL 数据源");
  subjectBrowser.record({
    id: "11-revoked-explicitly",
    acceptance: "F5-A2",
    description:
      "the confirmed revoke makes the open session 401 and returns it to login",
    screenshot: subjectBrowser.shot("11-revoked-explicitly"),
  });

  // The warning is what this step proves. Nothing is saved: saving the
  // signed-in admin would revoke this session and spend another login.
  const admins = zUserList.parse(
    (await api("GET", "/api/v1/users", adminCookie)).json,
  );
  const adminRow = admins.items.find(
    (item) => item.username === admin.username,
  );
  expect(adminRow !== undefined, "admin row missing");
  adminBrowser.open(`${origin}/users/detail?id=${adminRow?.id}`);
  adminBrowser.waitText("这是你正在使用的账号");
  adminBrowser.record({
    id: "12-self-warning",
    acceptance: "F5-A2",
    description:
      "editing the signed-in account warns that this session will be revoked",
    screenshot: adminBrowser.shot("12-self-warning"),
  });
  return adminCookie;
}

async function guardLastAdmin(
  origin: string,
  api: ApiClient,
  adminCookie: string,
): Promise<void> {
  adminBrowser.open(`${origin}/users/new`);
  adminBrowser.waitText("初始密码");
  adminBrowser.fill("用户名", secondAdmin.username);
  adminBrowser.fill("初始密码", secondAdmin.password);
  choose(adminBrowser, "角色", "管理员");
  adminBrowser.record({
    id: "13-create-second-admin",
    acceptance: "F5-A3",
    description:
      "a second administrator is created so the first can be guarded",
    screenshot: adminBrowser.shot("13-create-second-admin"),
  });
  adminBrowser.click("创建用户");
  adminBrowser.waitText("保存会吊销会话");
  const secondId = new URL(adminBrowser.url()).searchParams.get("id") ?? "";
  const before = zUser.parse(
    (await api("GET", `/api/v1/users/${secondId}`, adminCookie)).json,
  );
  expect(
    before.username === secondAdmin.username && before.role === "admin",
    "the created second administrator does not match the form",
  );

  adminBrowser.waitText("保存会吊销会话");
  choose(adminBrowser, "启用状态", "已禁用");
  adminBrowser.click("保存修改");
  adminBrowser.waitText("用户已更新");
  await until("the second admin to be disabled", 30_000, async () => {
    const row = zUser.parse(
      (await api("GET", `/api/v1/users/${secondId}`, adminCookie)).json,
    );
    return row.disabled ? row : undefined;
  });

  const admins = zUserList.parse(
    (await api("GET", "/api/v1/users", adminCookie)).json,
  );
  const adminRow = admins.items.find(
    (item) => item.username === admin.username,
  );
  expect(adminRow !== undefined, "admin row missing");
  const adminBefore = zUser.parse(
    (await api("GET", `/api/v1/users/${adminRow?.id}`, adminCookie)).json,
  );
  adminBrowser.open(`${origin}/users/detail?id=${adminRow?.id}`);
  adminBrowser.waitText("这是你正在使用的账号");
  choose(adminBrowser, "角色", "只读");
  adminBrowser.click("保存修改");
  adminBrowser.waitText("这是最后一个启用的管理员，不能被禁用或降级。");
  adminBrowser.record({
    id: "14-last-admin",
    acceptance: "F5-A3",
    description:
      "demoting the only enabled administrator shows the 409 explanation",
    screenshot: adminBrowser.shot("14-last-admin"),
  });
  const adminAfter = zUser.parse(
    (await api("GET", `/api/v1/users/${adminRow?.id}`, adminCookie)).json,
  );
  expect(
    adminAfter.role === adminBefore.role &&
      adminAfter.disabled === adminBefore.disabled &&
      adminAfter.updated_at === adminBefore.updated_at,
    "the last administrator changed despite the 409",
  );

  // Re-enable the second admin through the API. The page stays on the refused
  // change, whose role select would otherwise be submitted as the demotion.
  const restored = await api(
    "PATCH",
    `/api/v1/users/${secondId}`,
    adminCookie,
    {
      disabled: false,
    },
  );
  expect(restored.status === 200, `re-enable = ${restored.status}`);
}

async function checkAudit(
  origin: string,
  api: ApiClient,
  adminCookie: string,
): Promise<void> {
  // Padding past one page. Each create is itself an audited success, and the
  // comparison below is against every row the API returns, so the pad is
  // both the extra page and part of the expected set.
  for (let index = 0; index < 55; index += 1) {
    const created = await api("POST", "/api/v1/users", adminCookie, {
      username: `f5-pad-${String(index).padStart(2, "0")}`,
      password: randomPassword("pad"),
      role: "viewer",
    });
    expect(created.status === 201, `pad user ${index} = ${created.status}`);
  }
  const rows = await allAudit(api, adminCookie);
  expect(rows.length > 50, `audit rows = ${rows.length}`);
  const wanted = ["users.create", "users.update", "users.revoke_sessions"];
  for (const action of wanted) {
    expect(
      rows.some(
        (row) =>
          row.action === action &&
          row.result === "success" &&
          row.actor_name === admin.username &&
          row.resource_type === "user",
      ),
      `missing success audit ${action}`,
    );
  }

  adminBrowser.open(`${origin}/audit`);
  adminBrowser.waitText("加载更多");
  adminBrowser.waitText("创建用户");
  adminBrowser.record({
    id: "15-audit-first-page",
    acceptance: "F5-A4",
    description:
      "the audit log opens on the newest page and offers another page",
    screenshot: adminBrowser.shot("15-audit-first-page"),
  });
  adminBrowser.click("加载更多");
  adminBrowser.waitText("创建管理员");
  const rendered = z
    .array(z.string())
    .parse(
      JSON.parse(
        adminBrowser.ab(
          "eval",
          "(() => { const column = [...document.querySelectorAll('thead th')].findIndex((cell) => cell.textContent?.trim() === '请求 ID'); return [...document.querySelectorAll('tbody tr')].map((row) => row.children[column]?.textContent?.trim() ?? ''); })()",
        ),
      ),
    );
  // System audits (the bootstrap admin) carry no request ID; the page shows —.
  const expected = rows.map((row) => row.request_id || "—");
  expect(
    rendered.join("\n") === expected.join("\n"),
    `rendered audit request ids differ from the API (${rendered.length} vs ${expected.length}); first difference at ${rendered.findIndex((id, index) => id !== expected[index])}: ${rendered.find((id, index) => id !== expected[index])} vs ${expected.find((id, index) => id !== rendered[index])}`,
  );
  const html = pageHtml(adminBrowser);
  for (const secret of [
    admin.password,
    operator.password,
    viewer.password,
    resetPassword,
    secondAdmin.password,
  ]) {
    expect(!html.includes(secret), "a password is present in the audit page");
  }
  adminBrowser.record({
    id: "16-audit-paged",
    acceptance: "F5-A4",
    description:
      "the loaded pages match the API exactly, with no duplicate, gap or password",
    screenshot: adminBrowser.shot("16-audit-paged"),
  });
}

adminBrowser.writeManifest(run.commit, run.dirty, [subjectBrowser]);
console.log(
  `f5 e2e: ${adminBrowser.steps.length + subjectBrowser.steps.length} steps passed; evidence in ${run.evidence}${run.dirty ? " (working tree dirty)" : ""}`,
);
