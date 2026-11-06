import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { sessions } from "@/test/fixtures";
import {
  fetchMock,
  json,
  renderWithClient,
  routeFetch,
  user,
} from "@/test/render";
import { UserDetail } from "./user-detail";

const replace = vi.fn();
let search = new URLSearchParams({ id: user.id });

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
  useSearchParams: () => search,
}));

const operator = {
  ...user,
  id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a11",
  username: "operator.one",
  role: "operator" as const,
};

function requests(method: string) {
  return fetchMock.mock.calls
    .map(([input]) => input as Request)
    .filter((request) => request.method === method);
}

afterEach(() => {
  fetchMock.mockReset();
  replace.mockReset();
  search = new URLSearchParams({ id: user.id });
});

describe("UserDetail", () => {
  it("shows the fixed explanation for the last administrator and sends nothing else", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      "GET /api/v1/users/0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b": [
        () => json(200, user),
      ],
      "PATCH /api/v1/users/0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b": [
        () =>
          json(409, {
            error: {
              code: "conflict",
              message:
                "Cannot disable or demote the last enabled administrator",
              request_id: "req-409",
            },
          }),
      ],
    });
    renderWithClient(<UserDetail />);
    const actor = userEvent.setup();
    await actor.click(await screen.findByRole("combobox", { name: "角色" }));
    await actor.click(await screen.findByRole("option", { name: "只读" }));
    await actor.click(screen.getByRole("button", { name: "保存修改" }));

    expect(
      await screen.findByText(/这是最后一个启用的管理员，不能被禁用或降级/),
    ).toBeInTheDocument();
    expect(await requests("PATCH")[0]?.json()).toEqual({ role: "viewer" });
    expect(screen.getByRole("combobox", { name: "角色" })).toHaveTextContent(
      "只读",
    );
  });

  it("writes a reset password once and clears the field", async () => {
    search = new URLSearchParams({ id: operator.id });
    const updated = { ...operator, updated_at: "2026-11-06T09:00:00Z" };
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      [`GET /api/v1/users/${operator.id}`]: [
        () => json(200, operator),
        () => json(200, updated),
      ],
      [`PATCH /api/v1/users/${operator.id}`]: [() => json(200, updated)],
    });
    renderWithClient(<UserDetail />);
    const actor = userEvent.setup();
    const field = await screen.findByLabelText("重置密码");
    await actor.type(field, "replacement-pw");
    await actor.click(screen.getByRole("button", { name: "保存修改" }));

    await waitFor(() => expect(requests("PATCH")).toHaveLength(1));
    expect(await requests("PATCH")[0]?.json()).toEqual({
      password: "replacement-pw",
    });
    await waitFor(() => expect(field).toHaveValue(""));
    expect(
      screen.queryByDisplayValue("replacement-pw"),
    ).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("replacement-pw");
  });

  it("does not revoke sessions until the confirmation is accepted", async () => {
    search = new URLSearchParams({ id: operator.id });
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      [`GET /api/v1/users/${operator.id}`]: [() => json(200, operator)],
      [`DELETE /api/v1/users/${operator.id}/sessions`]: [
        () => new Response(null, { status: 204 }),
      ],
    });
    renderWithClient(<UserDetail />);
    const actor = userEvent.setup();
    await actor.click(
      await screen.findByRole("button", { name: "吊销全部会话" }),
    );
    expect(requests("DELETE")).toHaveLength(0);
    await actor.click(await screen.findByRole("button", { name: "确认吊销" }));

    await waitFor(() => expect(requests("DELETE")).toHaveLength(1));
    expect(requests("DELETE")[0]?.url).toContain(
      `/api/v1/users/${operator.id}/sessions`,
    );
  });

  it("warns that acting on the signed-in account ends this session", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      "GET /api/v1/users/0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b": [
        () => json(200, user),
      ],
    });
    renderWithClient(<UserDetail />);

    expect(
      (await screen.findAllByText(/这是你正在使用的账号/)).length,
    ).toBeGreaterThan(0);
  });
});
