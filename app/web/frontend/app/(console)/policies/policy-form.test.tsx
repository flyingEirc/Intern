import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { PolicyForm } from "./policy-form";

const push = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

afterEach(() => {
  fetchMock.mockReset();
  push.mockReset();
});

const policy = {
  id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b",
  name: "nightly",
  source_id: "source-1",
  local_repository_id: "repository-local",
  replica_repository_id: null,
  full_cron: "0 2 * * *",
  incremental_cron: "0 * * * *",
  timezone: "Asia/Shanghai",
  max_chain_length: 7,
  parallel: 1,
  throttle: 0,
  zstd_level: 3,
  page_tracking: false,
  enabled: true,
  last_preflight: null,
  created_at: "2026-11-06T08:00:00Z",
  updated_at: "2026-11-06T08:00:00Z",
};

describe("PolicyForm", () => {
  it("shows the preflight checks on 409 and keeps the entered values", async () => {
    routeFetch({
      "GET /api/v1/sources": [
        () =>
          json(200, {
            items: [
              {
                id: "source-1",
                name: "primary",
                socket: "/run/mysqld/mysqld.sock",
                datadir: "/var/lib/mysql",
                username: "backup",
                has_secret: true,
                server_uuid: null,
                server_version: null,
                last_preflight: null,
                created_at: "2026-11-06T08:00:00Z",
                updated_at: "2026-11-06T08:00:00Z",
              },
            ],
            next_before: null,
          }),
      ],
      "GET /api/v1/repositories": [
        () =>
          json(200, {
            items: [
              {
                id: "repository-local",
                name: "local-backups",
                kind: "local",
                tls_mode: "none",
                local: { root: "/backup/repository" },
                has_secret: false,
                plaintext_credentials: false,
                created_at: "2026-11-06T08:00:00Z",
                updated_at: "2026-11-06T08:00:00Z",
              },
            ],
            next_before: null,
          }),
      ],
      [`PUT /api/v1/policies/${policy.id}`]: [
        () =>
          json(409, {
            error: {
              code: "preflight_failed",
              message: "Policy preflight failed; the policy stays disabled",
              request_id: "req-preflight",
              preflight: {
                ok: false,
                checks: ["socket", "identity"],
                warnings: [
                  {
                    code: "binlog_format",
                    message: "binlog_format is not ROW",
                  },
                ],
                error: {
                  check: "space",
                  code: "insufficient_space",
                  message: "not enough free space",
                  missing: ["10GiB"],
                },
              },
            },
          }),
      ],
    });
    renderWithClient(<PolicyForm policy={policy} />);

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "保存修改" }));

    expect(await screen.findByText("保存被预检拒绝")).toBeInTheDocument();
    expect(screen.getByText("其他检查项")).toBeInTheDocument();
    expect(screen.getByText("数据库身份")).toBeInTheDocument();
    expect(screen.getByText("失败：其他检查项")).toBeInTheDocument();
    expect(screen.getByText(/not enough free space/)).toBeInTheDocument();
    expect(screen.getByText(/10GiB/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "预检诊断" }));
    expect(await screen.findByText("socket")).toBeInTheDocument();
    expect(screen.getByText(/space \/ insufficient_space/)).toBeInTheDocument();
    expect(screen.getByLabelText("名称")).toHaveValue("nightly");
    expect(push).not.toHaveBeenCalled();
  });

  it("shows the chosen names in the closed selects, not their ids", async () => {
    routeFetch({
      "GET /api/v1/sources": [
        () =>
          json(200, {
            items: [
              {
                id: "source-1",
                name: "primary",
                socket: "/run/mysqld/mysqld.sock",
                datadir: "/var/lib/mysql",
                username: "backup",
                has_secret: true,
                server_uuid: null,
                server_version: null,
                last_preflight: null,
                created_at: "2026-11-06T08:00:00Z",
                updated_at: "2026-11-06T08:00:00Z",
              },
            ],
            next_before: null,
          }),
      ],
      "GET /api/v1/repositories": [
        () =>
          json(200, {
            items: [
              {
                id: "repository-local",
                name: "local-backups",
                kind: "local",
                tls_mode: "none",
                local: { root: "/backup/repository" },
                has_secret: false,
                plaintext_credentials: false,
                created_at: "2026-11-06T08:00:00Z",
                updated_at: "2026-11-06T08:00:00Z",
              },
            ],
            next_before: null,
          }),
      ],
    });
    renderWithClient(
      <PolicyForm policy={{ ...policy, replica_repository_id: "none" }} />,
    );

    await waitFor(() =>
      expect(
        screen.getByRole("combobox", { name: "数据源" }),
      ).toHaveTextContent("primary"),
    );
    expect(
      screen.getByRole("combobox", { name: "本地仓库" }),
    ).toHaveTextContent("local-backups");
    expect(
      screen.getByRole("combobox", { name: "副本仓库（可选）" }),
    ).toHaveTextContent("不使用副本");
  });

  it.each([
    "30 4 1 1 *",
    "0 */5 * * *",
  ])("submits only business fields and preserves unmatched schedule %s", async (incrementalCron) => {
    routeFetch({
      "GET /api/v1/sources": [
        () => json(200, { items: [], next_before: null }),
      ],
      "GET /api/v1/repositories": [
        () => json(200, { items: [], next_before: null }),
      ],
      [`PUT /api/v1/policies/${policy.id}`]: [
        () => json(200, { ...policy, full_cron: "15 4 1 2 *" }),
      ],
    });
    renderWithClient(
      <PolicyForm
        policy={{
          ...policy,
          full_cron: "15 4 1 2 *",
          incremental_cron: incrementalCron,
        }}
      />,
    );

    expect(screen.getByLabelText("全量安排")).toHaveValue("15 4 1 2 *");
    expect(screen.getByLabelText("增量安排")).toHaveValue(incrementalCron);
    expect(screen.queryByLabelText("最大链长")).not.toBeInTheDocument();
    expect(screen.queryByText(/page tracking/i)).not.toBeInTheDocument();

    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "保存修改" }));

    await waitFor(() =>
      expect(push).toHaveBeenCalledWith(`/policies/detail?id=${policy.id}`),
    );
    const request = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((item) => item.method === "PUT");
    expect(await request?.json()).toEqual({
      name: "nightly",
      source_id: "source-1",
      local_repository_id: "repository-local",
      replica_repository_id: null,
      full_cron: "15 4 1 2 *",
      incremental_cron: incrementalCron,
      timezone: "Asia/Shanghai",
      enabled: true,
    });
  });
});
