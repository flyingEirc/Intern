import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchMock,
  json,
  renderWithClient,
  routeFetch,
  unauthorizedBody,
  user,
} from "@/test/render";
import { PasswordForm } from "./password-form";

const replace = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
}));

afterEach(() => {
  fetchMock.mockReset();
  replace.mockReset();
});

async function fill(current: string, next: string, confirm = next) {
  const u = userEvent.setup();
  await u.type(screen.getByLabelText("当前密码"), current);
  await u.type(screen.getByLabelText("新密码"), next);
  await u.type(screen.getByLabelText("确认新密码"), confirm);
  await u.click(screen.getByRole("button", { name: "修改密码" }));
}

const noContent = () => new Response(null, { status: 204 });

describe("PasswordForm", () => {
  it("keeps the rotated session and confirms the change", async () => {
    routeFetch({
      "PUT /api/v1/auth/password": [noContent],
      "GET /api/v1/auth/session": [
        () => json(200, { user, permissions: ["session:self"] }),
      ],
    });
    renderWithClient(<PasswordForm />);
    await fill("old-password-123", "new-password-4567");

    expect(await screen.findByText("密码已修改")).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
    const put = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((request) => request.method === "PUT");
    expect(await put?.json()).toEqual({
      current_password: "old-password-123",
      new_password: "new-password-4567",
    });
  });

  it("sends the user to log in again when rotation left no valid session", async () => {
    routeFetch({
      "PUT /api/v1/auth/password": [noContent],
      "GET /api/v1/auth/session": [() => json(401, unauthorizedBody)],
    });
    const { onUnauthorized } = renderWithClient(<PasswordForm />);
    await fill("old-password-123", "new-password-4567");

    await waitFor(() =>
      expect(replace).toHaveBeenCalledWith("/login?reason=password-changed"),
    );
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("marks the current password when the backend rejects it", async () => {
    routeFetch({
      "PUT /api/v1/auth/password": [
        () =>
          json(400, {
            error: {
              code: "validation_failed",
              message: "current password is incorrect",
              request_id: "req-3",
            },
          }),
      ],
    });
    renderWithClient(<PasswordForm />);
    await fill("not-my-password", "new-password-4567");

    expect(await screen.findByText("当前密码不正确")).toBeInTheDocument();
    expect(screen.getByLabelText("当前密码")).toHaveAttribute(
      "aria-invalid",
      "true",
    );
  });

  it("does not claim this session survived when it cannot be confirmed", async () => {
    routeFetch({
      "PUT /api/v1/auth/password": [noContent],
      "GET /api/v1/auth/session": [
        () =>
          json(
            503,
            {
              error: {
                code: "unavailable",
                message: "Metadata database is not ready",
                request_id: "req-503",
              },
            },
            { "Retry-After": "5" },
          ),
      ],
    });
    renderWithClient(<PasswordForm />);
    await fill("old-password-123", "new-password-4567");

    expect(
      await screen.findByText(/无法确认本会话是否仍然有效/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("本会话保持登录，其他会话已全部失效。"),
    ).not.toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
  });

  it("shows other 400 answers as reported instead of blaming the current password", async () => {
    routeFetch({
      "PUT /api/v1/auth/password": [
        () =>
          json(400, {
            error: {
              code: "invalid_json",
              message: "Request body is not valid JSON",
              request_id: "req-4",
            },
          }),
      ],
    });
    renderWithClient(<PasswordForm />);
    await fill("old-password-123", "new-password-4567");

    expect(
      await screen.findByText(/请求失败（HTTP 400，invalid_json）/),
    ).toBeInTheDocument();
    expect(screen.queryByText("当前密码不正确")).not.toBeInTheDocument();
  });

  it("does not send a new password that fails the shared rule", async () => {
    renderWithClient(<PasswordForm />);
    await fill("old-password-123", "short", "short");
    expect(
      await screen.findByText("新密码需为 12–256 个字符"),
    ).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
