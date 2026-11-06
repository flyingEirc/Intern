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

  it("does not send a new password that fails the shared rule", async () => {
    renderWithClient(<PasswordForm />);
    await fill("old-password-123", "short", "short");
    expect(
      await screen.findByText("新密码需为 12–256 个字符"),
    ).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
