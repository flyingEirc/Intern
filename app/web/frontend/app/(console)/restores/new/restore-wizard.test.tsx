import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  backupId,
  chain,
  planDigest,
  replica,
  replicaRepositoryId,
  repositoryId,
  restoreId,
  restorePlan,
  sessions,
  sourceId,
} from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { RestoreWizard } from "./restore-wizard";

const push = vi.fn();
const search = new URLSearchParams();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace: vi.fn() }),
  useSearchParams: () => search,
}));

const keys = ["11111111-1111-4111-8111-111111111111"];

beforeEach(() => {
  let issued = 0;
  vi.stubGlobal("crypto", { randomUUID: () => keys[issued++] ?? "extra" });
  vi.stubGlobal("navigator", {
    clipboard: { writeText: vi.fn().mockResolvedValue(undefined) },
  });
});

afterEach(() => {
  fetchMock.mockReset();
  push.mockReset();
  vi.unstubAllGlobals();
  for (const key of [...search.keys()]) {
    search.delete(key);
  }
});

const source = {
  id: sourceId,
  name: "生产库",
  socket: "/var/run/mysqld/mysqld.sock",
  datadir: "/var/lib/mysql",
  username: "intern_bkp",
  has_secret: true,
  server_uuid: null,
  server_version: null,
  last_preflight: null,
  created_at: "2026-11-06T08:00:00Z",
  updated_at: "2026-11-06T08:00:00Z",
};

function catalog(session = sessions.admin()) {
  return {
    "GET /api/v1/auth/session": [() => json(200, session)],
    "GET /api/v1/sources": [
      () => json(200, { items: [source], next_before: null }),
    ],
    [`GET /api/v1/sources/${sourceId}/chains`]: [
      () => json(200, { items: [chain()] }),
    ],
    "GET /api/v1/replicas": [
      () =>
        json(200, {
          items: [
            replica({
              status: "verified",
              alert_since: null,
              last_error_code: "",
              last_reply_code: 0,
            }),
          ],
        }),
    ],
    "GET /api/v1/repositories": [
      () =>
        json(200, {
          items: [
            {
              id: repositoryId,
              name: "本地目录",
              kind: "local",
              tls_mode: "none",
              local: { root: "/backup" },
              has_secret: false,
              plaintext_credentials: false,
              created_at: "2026-11-06T08:00:00Z",
              updated_at: "2026-11-06T08:00:00Z",
            },
            {
              id: replicaRepositoryId,
              name: "异地 FTP",
              kind: "ftp",
              tls_mode: "plain",
              ftp: {
                host: "ftp.internal",
                port: 21,
                base_path: "/backups",
                username: "replica",
              },
              has_secret: true,
              plaintext_credentials: true,
              created_at: "2026-11-06T08:00:00Z",
              updated_at: "2026-11-06T08:00:00Z",
            },
          ],
          next_before: null,
        }),
    ],
  };
}

function posts(path: string): Request[] {
  return fetchMock.mock.calls
    .map(([input]) => input as Request)
    .filter(
      (request) =>
        request.method === "POST" && new URL(request.url).pathname === path,
    );
}

async function choosePoint() {
  const user = userEvent.setup();
  await user.click(await screen.findByRole("combobox", { name: "数据源" }));
  await user.click(await screen.findByRole("option", { name: "生产库" }));
  await user.click(await screen.findByRole("radio", { name: chain().id }));
  await user.click(await screen.findByRole("radio", { name: "物理全量 1" }));
  return user;
}

describe("RestoreWizard", () => {
  it("shows point-in-time recovery as unavailable", async () => {
    routeFetch(catalog());
    renderWithClient(<RestoreWizard />);

    const option = await screen.findByRole("radio", {
      name: "任意时间点恢复（PITR）暂不可用",
    });
    expect(option).toHaveAttribute("aria-disabled", "true");
    expect(screen.getAllByText(/binlog 归档尚未实现/).length).toBeGreaterThan(
      0,
    );
  });

  it("shows each point's verified copies from the replica queue", async () => {
    // Chain members carry no replicas; the verified FTP copy must still show.
    routeFetch(catalog());
    renderWithClient(<RestoreWizard />);
    await choosePoint();
    expect(await screen.findByText("异地 FTP（已校验）")).toBeInTheDocument();
    const queue = fetchMock.mock.calls
      .map(([input]) => new URL((input as Request).url))
      .find((url) => url.pathname === "/api/v1/replicas");
    expect(queue?.searchParams.get("source_id")).toBe(sourceId);
  });

  it("renders each link's source and the Chinese risk explanations", async () => {
    routeFetch({
      ...catalog(),
      "POST /api/v1/restores/plan": [() => json(200, restorePlan())],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));

    expect(await screen.findByText("本地目录")).toBeInTheDocument();
    expect(screen.getByText("FTP 副本")).toBeInTheDocument();
    expect(screen.getByText("异地 FTP")).toBeInTheDocument();
    expect(screen.getByText("no_auto_replace_production")).toBeInTheDocument();
    expect(
      screen.getByText(/从不自动替换生产实例的 datadir/),
    ).toBeInTheDocument();
    expect(screen.getByText("pitr_unavailable")).toBeInTheDocument();
    expect(screen.getByText(/不是任意时间点恢复/)).toBeInTheDocument();
    expect(
      screen.getByText(planDigest.slice(0, 12), { exact: false }),
    ).toBeInTheDocument();
  });

  it("lets an operator plan but not reach confirmation", async () => {
    routeFetch({
      ...catalog(sessions.operator()),
      "POST /api/v1/restores/plan": [() => json(200, restorePlan())],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));

    expect(
      await screen.findByText("只有管理员可以执行恢复"),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText("管理员密码")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "执行恢复" }),
    ).not.toBeInTheDocument();
  });

  it("clears the password on submit and marks a failed confirmation", async () => {
    routeFetch({
      ...catalog(),
      "POST /api/v1/restores/plan": [() => json(200, restorePlan())],
      "POST /api/v1/restores": [
        () =>
          json(403, {
            error: {
              code: "confirmation_failed",
              message: "Password confirmation failed",
              request_id: "req-403",
            },
          }),
      ],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));
    const password = await screen.findByLabelText("管理员密码");
    await user.type(password, "secret-password");
    await user.click(screen.getByRole("button", { name: "执行恢复" }));

    await waitFor(() => expect(posts("/api/v1/restores")).toHaveLength(1));
    expect(password).toHaveValue("");
    expect(await password.getAttribute("aria-invalid")).toBe("true");
    expect(screen.getByText("密码确认失败，请重新输入。")).toBeInTheDocument();
    const body = await posts("/api/v1/restores")[0]?.json();
    expect(body.password).toBe("secret-password");
  });

  it("submits again after a refused password, without a new plan", async () => {
    routeFetch({
      ...catalog(),
      "POST /api/v1/restores/plan": [() => json(200, restorePlan())],
      "POST /api/v1/restores": [
        () =>
          json(403, {
            error: {
              code: "confirmation_failed",
              message: "Password confirmation failed",
              request_id: "req-403",
            },
          }),
        () => json(202, { restore_id: restoreId }),
      ],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));
    const password = await screen.findByLabelText("管理员密码");
    await user.type(password, "wrong-password");
    await user.click(screen.getByRole("button", { name: "执行恢复" }));
    expect(
      await screen.findByText("密码确认失败，请重新输入。"),
    ).toBeInTheDocument();

    await user.type(password, "secret-password");
    await user.click(screen.getByRole("button", { name: "执行恢复" }));

    await waitFor(() => expect(posts("/api/v1/restores")).toHaveLength(2));
    const second = await posts("/api/v1/restores")[1]?.json();
    expect(second.password).toBe("secret-password");
  });

  it("never resubmits a digest the server reported as changed", async () => {
    const fresh = "cd".repeat(32);
    routeFetch({
      ...catalog(),
      "POST /api/v1/restores/plan": [
        () => json(200, restorePlan()),
        () => json(200, restorePlan({ plan_digest: fresh })),
      ],
      "POST /api/v1/restores": [
        () =>
          json(409, {
            error: {
              code: "plan_changed",
              message: "The restore plan changed; request a fresh plan",
              request_id: "req-409",
            },
          }),
        () => json(202, { restore_id: restoreId }),
      ],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));
    const password = await screen.findByLabelText("管理员密码");
    await user.type(password, "secret-password");
    await user.click(screen.getByRole("button", { name: "执行恢复" }));

    expect(
      await screen.findByText(/原来确认的计划摘要已失效/),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "执行恢复" })).toBeDisabled();
    const replan = screen
      .getAllByRole("button", { name: "重新生成计划" })
      .at(-1);
    if (replan === undefined) {
      throw new Error("重新生成计划 button missing");
    }
    await user.click(replan);
    await screen.findByText(fresh.slice(0, 12), { exact: false });
    await user.type(screen.getByLabelText("管理员密码"), "secret-password");
    await user.click(screen.getByRole("button", { name: "执行恢复" }));

    await waitFor(() => expect(posts("/api/v1/restores").length).toBe(2));
    const bodies = await Promise.all(
      posts("/api/v1/restores").map((request) => request.clone().json()),
    );
    expect(bodies.map((body) => body.plan_digest)).toEqual([planDigest, fresh]);
    expect(push).toHaveBeenCalledWith(`/restores/detail?id=${restoreId}`);
  });

  it("blocks a submission whose verify_sql exceeds the engine limits", async () => {
    routeFetch({
      ...catalog(),
      "POST /api/v1/restores/plan": [() => json(200, restorePlan())],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));
    await user.type(
      await screen.findByLabelText("管理员密码"),
      "secret-password",
    );

    const editor = screen.getByLabelText("校验语句（verify_sql）");
    await user.click(editor);
    await user.paste(Array.from({ length: 17 }, () => "SELECT 1").join("\n"));
    await user.click(screen.getByRole("button", { name: "执行恢复" }));
    expect(
      await screen.findByText(
        "校验语句最多 16 条，当前 17 条。请删减后再执行。",
      ),
    ).toBeInTheDocument();
    expect(posts("/api/v1/restores")).toHaveLength(0);

    await user.clear(editor);
    await user.click(editor);
    await user.paste(`SELECT ${"x".repeat(4096)}`);
    await user.click(screen.getByRole("button", { name: "执行恢复" }));
    expect(
      await screen.findByText(/第 1 条：每条语句必须为 1 到 4096 字节/),
    ).toBeInTheDocument();
    expect(posts("/api/v1/restores")).toHaveLength(0);
  });

  it("sends one request when 执行恢复 is activated twice in one task", async () => {
    routeFetch({
      ...catalog(),
      "POST /api/v1/restores/plan": [() => json(200, restorePlan())],
      "POST /api/v1/restores": [
        () => {
          const { promise, resolve } = Promise.withResolvers<Response>();
          setTimeout(() => resolve(json(202, { restore_id: restoreId })), 50);
          return promise;
        },
      ],
    });
    renderWithClient(<RestoreWizard />);
    const user = await choosePoint();
    await user.click(screen.getByRole("button", { name: "生成计划" }));
    await user.type(
      await screen.findByLabelText("管理员密码"),
      "secret-password",
    );
    const button = screen.getByRole("button", { name: "执行恢复" });

    act(() => {
      button.click();
      button.click();
    });

    await waitFor(() => expect(posts("/api/v1/restores")).toHaveLength(1));
    const sent = posts("/api/v1/restores");
    const first = sent[0];
    if (first === undefined) {
      throw new Error("no restore request");
    }
    expect(first.headers.get("Idempotency-Key")).toBe(keys[0]);
    expect((await first.json()).plan_digest).toBe(planDigest);
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith(`/restores/detail?id=${restoreId}`),
    );
  });
});

void backupId;
