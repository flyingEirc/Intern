// End-to-end acceptance of the embedded UI (F1 #54), driven by agent-browser
// against a real bin/intern-web built with the webui tag (`mise run build`).
// Every assertion reads observable state: the browser URL and rendered text,
// HTTP answers of the same server, and the session store through the API.
//
//   bun e2e/run.ts [--evidence DIR] [--headed]
//
// Screenshots go to DIR (default <repo>/.cache/e2e/<commit>) with a
// manifest.json listing each step. Test passwords are random per run and are
// never written to the evidence.
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import {
  zCreateUserResponse,
  zErrorResponse,
} from "../lib/api/generated/zod.gen";
import { startNative, stopOnSignal } from "./harness";

const repo = path.resolve(import.meta.dir, "../../../..");
const args = process.argv.slice(2);
const headed = args.includes("--headed");
const commit =
  spawnSync("git", ["rev-parse", "--short=12", "HEAD"], {
    cwd: repo,
    encoding: "utf8",
  }).stdout.trim() || "unknown";
const dirty =
  spawnSync("git", ["status", "--porcelain"], {
    cwd: repo,
    encoding: "utf8",
  }).stdout.trim() !== "";
const evidenceIndex = args.indexOf("--evidence");
const evidence =
  evidenceIndex >= 0
    ? path.resolve(args[evidenceIndex + 1])
    : path.join(repo, ".cache/e2e", commit);

type Step = {
  id: string;
  acceptance: string;
  description: string;
  screenshot?: string;
  result: "pass";
};
const steps: Step[] = [];

function fail(message: string): never {
  throw new Error(`e2e: ${message}`);
}

function expect(condition: boolean, message: string): void {
  if (!condition) {
    fail(message);
  }
}

// ---------------------------------------------------------------- server ---

const native = await startNative({ setupInBrowser: true });
const origin = native.origin;
stopOnSignal(native.stop);

const password = () => `e2e-${randomBytes(12).toString("base64url")}`;
const adminPassword = password();
const operator = {
  username: "e2e-operator",
  password: password(),
  newPassword: password(),
};

// ------------------------------------------------------------------- API ---

async function apiLogin(
  username: string,
  secret: string,
): Promise<{ status: number; cookie?: string }> {
  const response = await fetch(`${origin}/api/v1/auth/login`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username, password: secret }),
  });
  const cookie = response.headers
    .getSetCookie()
    .find((value) => value.startsWith("intern_session="))
    ?.split(";")[0];
  await response.arrayBuffer();
  return { status: response.status, cookie };
}

async function api(
  method: string,
  route: string,
  cookie: string,
  body?: unknown,
): Promise<Response> {
  return fetch(`${origin}${route}`, {
    method,
    headers: {
      Cookie: cookie,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

// --------------------------------------------------------------- browser ---

const session = `intern-f1-e2e-${process.pid}`;

function ab(...command: string[]): string {
  const result = spawnSync(
    "agent-browser",
    ["--session", session, ...(headed ? ["--headed"] : []), ...command],
    { encoding: "utf8", timeout: 60_000 },
  );
  if (result.status !== 0) {
    fail(
      `agent-browser ${command[0]} failed: ${(result.stderr || result.stdout).trim()}`,
    );
  }
  return result.stdout.trim();
}

function url(): string {
  return ab("get", "url");
}

function shot(name: string): string {
  const file = path.join(evidence, `${name}.png`);
  // Capture settled frames: toasts and menus animate in over ~500 ms.
  ab(
    "wait",
    "--fn",
    "document.getAnimations().every((a) => a.playState !== 'running')",
  );
  ab("screenshot", file);
  return path.basename(file);
}

function record(step: Omit<Step, "result">): void {
  steps.push({ ...step, result: "pass" });
  console.log(`pass ${step.id} (${step.acceptance}): ${step.description}`);
}

async function staticChecks(): Promise<void> {
  const index = await fetch(`${origin}/`);
  const html = await index.text();
  expect(index.status === 200, `GET / = ${index.status}`);
  expect(
    index.headers.get("content-type") === "text/html; charset=utf-8",
    "GET / content type",
  );
  expect(
    index.headers.get("cache-control") === "no-cache",
    "GET / must revalidate",
  );
  expect(
    (index.headers.get("content-security-policy") ?? "").includes(
      "frame-ancestors 'none'",
    ),
    "GET / carries the CSP",
  );
  const asset =
    html.match(/\/_next\/static\/[^"']+\.js/)?.[0] ??
    fail("index.html references no script");
  const script = await fetch(`${origin}${asset}`);
  await script.arrayBuffer();
  expect(script.status === 200, `GET ${asset} = ${script.status}`);
  expect(
    script.headers.get("content-type") === "text/javascript; charset=utf-8",
    "script content type",
  );
  expect(
    script.headers.get("cache-control") ===
      "public, max-age=31536000, immutable",
    "asset cache",
  );
  const apiMissing = await fetch(`${origin}/api/v1/does-not-exist`);
  const apiBody = zErrorResponse.parse(await apiMissing.json());
  expect(
    apiMissing.status === 404 && apiBody.error.code === "not_found",
    "unknown /api path keeps JSON 404",
  );
  const pageMissing = await fetch(`${origin}/no-such-page`);
  expect(
    pageMissing.status === 404 &&
      (await pageMissing.text()).includes("页面不存在"),
    "unknown page answers the export 404",
  );
  const post = await fetch(`${origin}/login`, { method: "POST" });
  await post.arrayBuffer();
  expect(
    post.status === 405 && post.headers.get("allow") === "GET, HEAD",
    "POST to a page is 405",
  );
  record({
    id: "static",
    acceptance: "F1-A4",
    description: `same-origin pages and ${asset} with immutable cache; /api 404 stays JSON; page 404; POST 405`,
  });
}

async function main(): Promise<void> {
  mkdirSync(evidence, { recursive: true });
  await staticChecks();
  ab("set", "viewport", "1440", "900");
  ab("open", `${origin}/login`);
  ab("wait", "--text", "创建首位管理员");
  ab("find", "label", "管理员用户名", "fill", "e2e-admin");
  ab("find", "label", "设置密码", "fill", adminPassword);
  ab("find", "label", "确认密码", "fill", adminPassword);
  ab("find", "role", "button", "click", "--name", "创建管理员并进入控制台");
  ab("wait", "--text", "数据保护概览");
  record({
    id: "00-browser-setup",
    acceptance: "S1/U3",
    description:
      "first administrator created only through the browser on a real PG-backed Web server",
    screenshot: shot("00-browser-setup"),
  });
  ab("find", "role", "button", "click", "--name", "账户菜单");
  ab("wait", "--text", "退出登录");
  ab("find", "role", "menuitem", "click", "--name", "退出登录");
  ab("wait", "--text", "使用 Intern 账号登录控制台");

  const admin = await apiLogin("e2e-admin", adminPassword);
  expect(admin.status === 200 && admin.cookie !== undefined, "admin API login");
  const created = await api("POST", "/api/v1/users", admin.cookie as string, {
    username: operator.username,
    password: operator.password,
    role: "operator",
  });
  expect(created.status === 201, `create operator = ${created.status}`);
  const operatorId = zCreateUserResponse.parse(await created.json()).id;

  ab("set", "viewport", "1280", "800");

  // F1-A5: a protected page without a session lands on the login page.
  ab("open", `${origin}/account`);
  ab("wait", "--url", "**/login?next=%2Faccount");
  ab("wait", "--text", "使用 Intern 账号登录控制台");
  ab("find", "label", "用户名", "fill", operator.username);
  record({
    id: "01-login-redirect",
    acceptance: "F1-A5",
    description: "unauthenticated /account redirects to /login?next=%2Faccount",
    screenshot: shot("01-login-redirect"),
  });

  ab("find", "label", "密码", "fill", `${operator.password}-wrong`);
  ab("find", "role", "button", "click", "--name", "登录");
  ab("wait", "--text", "用户名或密码错误。");
  expect(
    url().endsWith("/login?next=%2Faccount"),
    "wrong credentials stay on the login page",
  );
  record({
    id: "02-login-failed",
    acceptance: "F1-A5",
    description:
      "wrong password shows the fixed error and does not enter the console",
    screenshot: shot("02-login-failed"),
  });

  ab("find", "label", "密码", "fill", operator.password);
  ab("find", "role", "button", "click", "--name", "登录");
  ab("wait", "--url", "**/account");
  ab("wait", "--text", "确认新密码");
  record({
    id: "03-login-returns",
    acceptance: "F1-A5",
    description: "correct credentials return to the requested /account",
    screenshot: shot("03-account"),
  });

  ab("find", "role", "link", "click", "--name", "概览");
  ab("wait", "--text", "数据保护概览");
  ab("wait", "--text", "就绪");
  const overview = ab("get", "text", "body");
  expect(
    overview.includes(operator.username) && overview.includes("操作员"),
    "overview shows the operator",
  );
  ab("find", "role", "link", "click", "--name", "账户与密码");
  ab("wait", "--text", "有效权限");
  ab("find", "role", "button", "click", "--name", "有效权限");
  const account = ab("get", "text", "body");
  expect(
    account.includes("runs:trigger") && !account.includes("users:manage"),
    "operator permissions only",
  );
  record({
    id: "04-overview",
    acceptance: "F1-A5",
    description:
      "overview shows readiness and the account page shows the session user, role and effective permissions",
    screenshot: shot("04-overview"),
  });

  // F1-A6: an administrator revokes the operator's sessions; the next API
  // call from the open page is 401 and the UI returns to login with `next`.
  ab("find", "role", "link", "click", "--name", "账户与密码");
  ab("wait", "--text", "确认新密码");
  const revoked = await api(
    "DELETE",
    `/api/v1/users/${operatorId}/sessions`,
    admin.cookie as string,
  );
  expect(revoked.status === 204, `revoke sessions = ${revoked.status}`);
  ab("find", "label", "当前密码", "fill", operator.password);
  ab("find", "label", "新密码", "fill", operator.newPassword);
  ab("find", "label", "确认新密码", "fill", operator.newPassword);
  ab("find", "role", "button", "click", "--name", "修改密码");
  ab("wait", "--url", "**/login?next=%2Faccount&reason=expired");
  ab("wait", "--text", "会话已失效，请重新登录。");
  record({
    id: "05-session-expired",
    acceptance: "F1-A6",
    description:
      "after DELETE /users/{id}/sessions the next request is 401 and the UI redirects with reason=expired",
    screenshot: shot("05-session-expired"),
  });

  ab("find", "label", "用户名", "fill", operator.username);
  ab("find", "label", "密码", "fill", operator.password);
  ab("find", "role", "button", "click", "--name", "登录");
  ab("wait", "--url", "**/account");
  ab("wait", "--text", "确认新密码");
  record({
    id: "06-relogin-returns",
    acceptance: "F1-A6",
    description: "logging in again returns to the page the session expired on",
  });

  // F1-A7: change the password, then sign out.
  ab("find", "label", "当前密码", "fill", operator.password);
  ab("find", "label", "新密码", "fill", operator.newPassword);
  ab("find", "label", "确认新密码", "fill", operator.newPassword);
  ab("find", "role", "button", "click", "--name", "修改密码");
  ab("wait", "--text", "密码已修改");
  record({
    id: "07-password-changed",
    acceptance: "F1-A7",
    description: "password change succeeds and this session stays signed in",
    screenshot: shot("07-password-changed"),
  });
  // The login limiter allows 5 attempts/minute per IP + username; this run
  // makes exactly 5 for the operator (2 browser, 1 re-login, these 2).
  const oldLogin = await apiLogin(operator.username, operator.password);
  expect(oldLogin.status === 401, `old password answers ${oldLogin.status}`);
  const newLogin = await apiLogin(operator.username, operator.newPassword);
  expect(newLogin.status === 200, `new password answers ${newLogin.status}`);
  ab("find", "role", "link", "click", "--name", "概览");
  ab("wait", "--text", "数据保护概览");
  expect(
    url() === `${origin}/`,
    "the rotated session still reaches the console",
  );

  // Two full page loads, so that Back after logout restores a document from
  // the back-forward cache rather than re-rendering a client-side route.
  ab("open", `${origin}/account`);
  ab("wait", "--text", "确认新密码");
  ab("open", `${origin}/`);
  ab("wait", "--text", "数据保护概览");
  const cookies = z
    .object({
      data: z.object({
        cookies: z.array(z.object({ name: z.string(), value: z.string() })),
      }),
    })
    .parse(JSON.parse(ab("cookies", "--json")));
  const browserCookie = cookies.data.cookies.find(
    (cookie) => cookie.name === "intern_session",
  );
  expect(browserCookie !== undefined, "browser holds the session cookie");
  ab("find", "role", "button", "click", "--name", "账户菜单");
  ab("wait", "--text", "退出登录");
  ab("find", "role", "menuitem", "click", "--name", "退出登录");
  ab("wait", "--url", "**/login?reason=signed-out");
  ab("wait", "--text", "已退出登录。");
  const stale = await api(
    "GET",
    "/api/v1/auth/session",
    `intern_session=${browserCookie?.value}`,
  );
  expect(
    stale.status === 401,
    `the signed-out cookie still answers ${stale.status}`,
  );
  record({
    id: "08-signed-out",
    acceptance: "F1-A7",
    description:
      "logout returns to login and the previous cookie is rejected by the API",
    screenshot: shot("08-signed-out"),
  });

  ab("back");
  ab("wait", "--url", "**/login?next=%2Faccount");
  ab("wait", "--text", "使用 Intern 账号登录控制台");
  expect(
    !ab("get", "text", "body").includes(operator.username),
    "Back after logout still shows the previous user's page",
  );
  record({
    id: "08b-back-after-logout",
    acceptance: "F1-A7",
    description:
      "Back after logout does not show the previous user's page; it lands on login",
  });

  const errors = ab("errors");
  expect(errors === "", `page errors: ${errors}`);
  const consoleOutput = ab("console");
  expect(
    !/Content Security Policy|Refused to/i.test(consoleOutput),
    `CSP violations: ${consoleOutput}`,
  );
  record({
    id: "09-clean-console",
    acceptance: "F1-A4",
    description: "no page errors or CSP violations",
  });
}

try {
  await main();
  writeFileSync(
    path.join(evidence, "manifest.json"),
    `${JSON.stringify({ commit, dirty, origin, steps }, null, 2)}\n`,
  );
  console.log(
    `e2e: ${steps.length} steps passed; evidence in ${evidence}${dirty ? " (working tree dirty)" : ""}`,
  );
} finally {
  spawnSync("agent-browser", ["--session", session, "close"], {
    encoding: "utf8",
  });
  await native.stop();
}
