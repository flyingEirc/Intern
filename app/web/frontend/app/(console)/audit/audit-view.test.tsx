import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import type { AuditLog } from "@/lib/api/generated/types.gen";
import { sessions } from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { AuditView } from "./audit-view";

function entry(id: number, action: string): AuditLog {
  return {
    id,
    ts: "2026-11-06T08:00:00Z",
    actor_type: "user",
    actor_id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b",
    actor_name: "admin",
    action,
    resource_type: "user",
    resource_id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a11",
    ip: "127.0.0.1",
    result: "success",
    status: 200,
    request_id: `req-${id}`,
    detail: { role: "operator", password_changed: id === 2 },
  };
}

function auditRequests() {
  return fetchMock.mock.calls
    .map(([input]) => input as Request)
    .filter(
      (request) => new URL(request.url).pathname === "/api/v1/audit-logs",
    );
}

afterEach(() => {
  fetchMock.mockReset();
});

describe("AuditView", () => {
  it("asks for the next page with the returned before cursor", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      "GET /api/v1/audit-logs": [
        () =>
          json(200, {
            items: [entry(3, "users.create"), entry(2, "users.update")],
            next_before: 2,
          }),
        () => json(200, { items: [entry(1, "auth.login")], next_before: null }),
      ],
    });
    renderWithClient(<AuditView />);

    expect(await screen.findByText("创建用户")).toBeInTheDocument();
    expect(screen.getByText("修改用户")).toBeInTheDocument();
    expect(
      screen.getByText("角色：操作员，密码已更改：是"),
    ).toBeInTheDocument();
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "加载更多" }));

    await waitFor(() => expect(auditRequests()).toHaveLength(2));
    const second = new URL(auditRequests()[1]?.url ?? "http://unused");
    expect(second.searchParams.get("before")).toBe("2");
    expect(second.searchParams.get("limit")).toBe("50");
    expect(await screen.findByText("登录")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "加载更多" }),
    ).not.toBeInTheDocument();
  });

  it("shows the no-permission state without requesting the audit log", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.viewer())],
    });
    renderWithClient(<AuditView />);

    expect(await screen.findByText("没有权限")).toBeInTheDocument();
    expect(auditRequests()).toHaveLength(0);
  });
});
