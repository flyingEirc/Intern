import { mkdirSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import {
  apiLogin,
  Browser,
  expect,
  makeApi,
  randomPassword,
  runContext,
  startNative,
  stopOnSignal,
} from "./harness";

// Real PG + embedded webui evidence, independent of the physical backup stack.
const run = runContext("responsive");
const b = new Browser(
  `intern-responsive-${process.pid}`,
  run.evidence,
  run.headed,
);
let race: Browser | undefined;
const server = await startNative({ setupInBrowser: true });
stopOnSignal(server.stop);
const password = randomPassword("responsive");
try {
  b.open(`${server.origin}/login`);
  b.waitText("创建首位管理员");
  b.record({
    id: "setup",
    acceptance: "U2",
    description: "First administrator setup on real PostgreSQL",
    screenshots: b.responsive("setup"),
  });
  b.ab("focus", "#setup-username");
  b.ab("type", "#setup-username", "ui-review-admin");
  b.ab("press", "Tab");
  b.ab("type", ":focus", password);
  b.ab("press", "Tab");
  b.ab("type", ":focus", password);
  b.ab("press", "Tab");
  b.ab(
    "wait",
    "--fn",
    "document.activeElement?.textContent?.includes('创建管理员') === true",
  );
  b.ab("press", "Enter");
  b.waitText("数据保护概览");
  b.record({
    id: "setup-keyboard",
    acceptance: "U3",
    description: "Setup completed by keyboard focus, Tab and Enter",
  });
  const pages = [
    ["overview", "/", "数据保护概览"],
    ["sources", "/sources", "数据源"],
    ["source-new", "/sources/new", "创建数据源"],
    ["repositories", "/repositories", "备份仓库"],
    ["repository-new", "/repositories/new", "创建仓库"],
    ["policies", "/policies", "保护策略"],
    ["policy-new", "/policies/new", "创建策略"],
    ["runs", "/runs", "运行记录"],
    ["replicas", "/replicas", "异地副本"],
    ["restores", "/restores", "新建恢复"],
    ["restore-wizard", "/restores/new", "新建恢复"],
    ["users", "/users", "用户列表"],
    ["user-new", "/users/new", "新建用户"],
    ["audit", "/audit", "审计记录"],
    ["account", "/account", "修改密码"],
    [
      "source-error",
      "/sources/detail?id=00000000-0000-7000-8000-000000000000",
      "无法读取数据源",
    ],
    [
      "repository-error",
      "/repositories/detail?id=00000000-0000-7000-8000-000000000000",
      "无法读取仓库",
    ],
    [
      "policy-error",
      "/policies/detail?id=00000000-0000-7000-8000-000000000000",
      "无法读取策略",
    ],
  ];
  for (const [name, route, text] of pages) {
    b.open(`${server.origin}${route}`);
    b.waitText(text as string);
    b.ab("wait", "--fn", "document.querySelector('[aria-busy=true]') === null");
    b.record({
      id: name as string,
      acceptance: "U2",
      description: `${route}: 375/768/1440, no horizontal page overflow`,
      screenshots: b.responsive(name as string),
    });
    b.ab("press", "Tab");
    b.ab(
      "wait",
      "--fn",
      "document.activeElement !== document.body && getComputedStyle(document.activeElement).visibility !== 'hidden'",
    );
    b.assertCleanConsole();
  }
  b.open(`${server.origin}/policies/new`);
  b.waitText("还没有数据源");
  b.waitText("添加数据源");
  b.waitText("还没有本地仓库");
  expect(
    b.ab(
      "eval",
      "document.querySelector('#policy-source')?.hasAttribute('disabled') === true && document.querySelector('#policy-local-repository')?.hasAttribute('disabled') === true",
    ) === "true",
    "the empty policy choices are still interactive",
  );
  b.record({
    id: "empty-policy-choices",
    acceptance: "U2/U3",
    description:
      "an empty instance disables both required policy selects and points to adding a source and a local repository",
    screenshots: b.responsive("empty-policy-choices"),
  });
  b.open(`${server.origin}/restores/new`);
  b.waitText("还没有可恢复的备份");
  b.waitText("查看保护策略");
  b.waitText("任意时间点恢复（暂不支持）");
  b.waitText("当前版本只能恢复到已完成的备份时间点。");
  expect(
    !/binlog|路线图|检查点|备份链/.test(b.text()),
    "the recovery wizard still uses implementation language",
  );
  b.record({
    id: "empty-restore-choices",
    acceptance: "U2/U3",
    description:
      "an empty instance offers a next step from the recovery wizard and uses only business recovery language",
    screenshots: b.responsive("empty-restore-choices"),
  });
  b.open(`${server.origin}/runs`);
  b.waitText("还没有运行记录");
  expect(
    b.ab("eval", "document.querySelector('#runs-status') === null") === "true",
    "the empty run list still shows filters",
  );
  b.open(`${server.origin}/`);
  b.waitText("数据保护概览");
  expect(
    b.ab(
      "eval",
      "Array.from(document.querySelectorAll('button')).find((element) => element.textContent?.includes('新建恢复'))?.disabled === true",
    ) === "true",
    "the empty instance still enables 新建恢复",
  );
  expect(
    b.ab(
      "eval",
      "document.querySelector('a[href=\"/policies/new\"]')?.getAttribute('role') === 'link'",
    ) === "true",
    "pure navigation is not announced as a link",
  );
  expect(
    b.ab(
      "eval",
      "Array.from(document.querySelectorAll('button,[role=button]')).filter((element) => (element.textContent?.trim() === '切换侧边栏' || element.getAttribute('aria-label') === '切换侧边栏') && element.tabIndex >= 0).length",
    ) === "1",
    "more than one focusable sidebar trigger has the same name",
  );
  const api = makeApi(server.origin);
  const cookie = await apiLogin(api, "ui-review-admin", password);
  expect(cookie !== undefined, "administrator API login for the state checks");
  const repositoryRoot = mkdtempSync(path.join(tmpdir(), "intern-ui-local-"));
  mkdirSync(repositoryRoot, { recursive: true });
  const local = await api("POST", "/api/v1/repositories", cookie, {
    name: "review-local",
    kind: "local",
    tls_mode: "none",
    root: repositoryRoot,
  });
  expect(local.status === 201, `local repository = ${local.status}`);
  b.open(`${server.origin}/`);
  b.waitText("开始保护数据库");
  const completedSteps = Number(
    b.ab(
      "eval",
      "Array.from(document.querySelectorAll('span, div'), (element) => element.children.length === 0 ? element.textContent?.trim() : null).filter((text) => text === '已完成').length",
    ),
  );
  expect(
    completedSteps === 1,
    `a local repository must complete only the storage step, ${completedSteps} steps are marked done`,
  );
  const storageStep = b.ab(
    "eval",
    "Array.from(document.querySelectorAll('div')).find((element) => element.textContent?.includes('2 · 配置备份仓库') && element.children.length === 3)?.textContent ?? ''",
  );
  expect(
    storageStep.includes("已完成"),
    `the storage step is not marked done: ${storageStep}`,
  );
  b.record({
    id: "onboarding-storage",
    acceptance: "U2/U3",
    description:
      "with one local repository only, the onboarding marks the storage step done and still offers the source and policy steps",
    screenshot: b.shot("onboarding-storage"),
  });

  b.open(`${server.origin}/policies/new`);
  b.waitText("保护时间安排");
  b.click("每小时");
  b.waitText("在第几分钟");
  expect(
    !b.text().includes("仅使用输入时间的分钟部分"),
    "the hourly schedule still explains cron implementation",
  );
  const hourlyTimes = Number(
    b.ab(
      "eval",
      "Array.from(document.querySelectorAll('label'), (element) => element.textContent?.trim()).filter((text) => text === '执行时间').length",
    ),
  );
  expect(hourlyTimes === 1, `hourly shows ${hourlyTimes} HH:mm controls`);
  b.record({
    id: "schedule-hourly",
    acceptance: "U3",
    description:
      "the hourly schedule offers an interval and a minute of the hour, and keeps HH:mm only for the full backup",
    screenshot: b.shot("schedule-hourly"),
  });
  const clicked = b.ab(
    "eval",
    "(() => { const group = document.querySelector('[aria-label=\"增量保护频率\"]'); const item = Array.from(group?.querySelectorAll('button') ?? []).find((button) => button.textContent?.trim() === '每天'); item?.click(); return item !== undefined; })()",
  );
  expect(clicked === "true", "the incremental daily choice was not found");
  b.waitText("执行时间");
  const dailyTimes = Number(
    b.ab(
      "eval",
      "Array.from(document.querySelectorAll('label'), (element) => element.textContent?.trim()).filter((text) => text === '执行时间').length",
    ),
  );
  expect(dailyTimes === 2, `daily shows ${dailyTimes} HH:mm controls`);
  b.signOut();
  b.record({
    id: "login",
    acceptance: "U2",
    description: "Initialized login at all required viewports",
    screenshots: b.responsive("login"),
  });
  console.log(`Evidence: ${run.evidence}`);
  const raceConnect = await startNative({});
  race = new Browser(
    `intern-setup-race-${process.pid}`,
    run.evidence,
    run.headed,
  );
  const racePassword = randomPassword("race");
  try {
    race.open(`${raceConnect.origin}/login`);
    race.waitText("创建首位管理员");
    const created = await makeApi(raceConnect.origin)(
      "POST",
      "/api/v1/auth/setup",
      undefined,
      { username: "race-admin", password: racePassword },
    );
    expect(created.status === 201, `concurrent setup = ${created.status}`);
    race.fill("管理员用户名", "race-admin");
    race.fill("设置密码", racePassword);
    race.fill("确认密码", racePassword);
    race.click("创建管理员并进入控制台");
    race.waitText("初始化已完成");
    race.ab("find", "role", "link", "click", "--name", "前往登录");
    race.waitText("使用 Intern 账号登录控制台");
    race.fill("用户名", "race-admin");
    race.fill("密码", racePassword);
    race.click("登录");
    race.waitText("数据保护概览");
    race.record({
      id: "setup-completed-elsewhere",
      acceptance: "U3",
      description:
        "a setup submitted after another request created the administrator shows the completed state, offers sign-in and that account reaches the console",
      screenshots: race.responsive("setup-completed-elsewhere"),
    });
    race.assertCleanConsole();
  } finally {
    race.close();
    await raceConnect.stop();
  }
} finally {
  b.writeManifest(run.commit, run.dirty, race ? [race] : []);
  b.close();
  await server.stop();
}
