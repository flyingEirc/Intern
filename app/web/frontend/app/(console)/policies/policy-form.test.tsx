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

    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "保存修改" }));

    expect(await screen.findByText("保存被预检拒绝")).toBeInTheDocument();
    expect(screen.getByText("socket")).toBeInTheDocument();
    expect(screen.getByText("identity")).toBeInTheDocument();
    expect(screen.getByText("失败：space")).toBeInTheDocument();
    expect(screen.getByText("insufficient_space")).toBeInTheDocument();
    expect(screen.getByText(/10GiB/)).toBeInTheDocument();
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

    // The trigger of a closed Base UI select renders the selected label once
    // the choices have loaded, never the raw id.
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
});
