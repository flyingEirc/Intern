import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  fetchMock,
  json,
  renderWithClient,
  routeFetch,
  unauthorizedBody,
  user,
} from "@/test/render";
import { LoginForm } from "./login-form";

const replace = vi.fn();
let search = new URLSearchParams();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
  useSearchParams: () => search,
}));

beforeEach(() => {
  search = new URLSearchParams();
});

afterEach(() => {
  fetchMock.mockReset();
  replace.mockReset();
});

async function submit(username: string, password: string) {
  const u = userEvent.setup();
  await u.type(screen.getByLabelText("用户名"), username);
  await u.type(screen.getByLabelText("密码"), password);
  await u.click(screen.getByRole("button", { name: "登录" }));
}

describe("LoginForm", () => {
  it("signs in and returns to the requested page", async () => {
    search = new URLSearchParams({ next: "/account", reason: "expired" });
    routeFetch({
      "GET /api/v1/auth/session": [() => json(401, unauthorizedBody)],
      "GET /api/v1/auth/setup": [() => json(200, { required: false })],
      "POST /api/v1/auth/login": [() => json(200, { user })],
    });
    const { onUnauthorized } = renderWithClient(<LoginForm />);
    expect(
      await screen.findByText("会话已失效，请重新登录。"),
    ).toBeInTheDocument();

    await submit("admin", "correct-horse-battery");

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/account"));
    const login = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((request) => request.method === "POST");
    expect(await login?.json()).toEqual({
      username: "admin",
      password: "correct-horse-battery",
    });
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("routes a first visit to setup", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(401, unauthorizedBody)],
      "GET /api/v1/auth/setup": [() => json(200, { required: true })],
    });
    renderWithClient(<LoginForm />);
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/setup"));
  });

  it("shows the fixed message for wrong credentials and stays", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(401, unauthorizedBody)],
      "GET /api/v1/auth/setup": [() => json(200, { required: false })],
      "POST /api/v1/auth/login": [
        () =>
          json(401, {
            error: {
              code: "unauthorized",
              message: "Invalid username or password",
              request_id: "req-1",
            },
          }),
      ],
    });
    const { onUnauthorized } = renderWithClient(<LoginForm />);
    await submit("admin", "wrong-password");

    expect(await screen.findByText("用户名或密码错误。")).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("tells the user when to retry after rate limiting", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(401, unauthorizedBody)],
      "GET /api/v1/auth/setup": [() => json(200, { required: false })],
      "POST /api/v1/auth/login": [
        () =>
          json(
            429,
            {
              error: {
                code: "rate_limited",
                message: "Too many login attempts",
                request_id: "req-2",
              },
            },
            { "Retry-After": "30" },
          ),
      ],
    });
    renderWithClient(<LoginForm />);
    await submit("admin", "wrong-password");
    expect(
      await screen.findByText("尝试过于频繁，请 30 秒后重试"),
    ).toBeInTheDocument();
  });

  it("validates input before sending anything", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(401, unauthorizedBody)],
      "GET /api/v1/auth/setup": [() => json(200, { required: false })],
    });
    renderWithClient(<LoginForm />);
    await userEvent.setup().click(screen.getByRole("button", { name: "登录" }));
    expect(await screen.findByText("请输入用户名")).toBeInTheDocument();
    expect(screen.getByText("请输入密码")).toBeInTheDocument();
    expect(
      fetchMock.mock.calls.some(
        ([input]) => (input as Request).method === "POST",
      ),
    ).toBe(false);
  });

  it("sends an already signed-in visitor on without a form submit", async () => {
    search = new URLSearchParams({ next: "//evil.example/" });
    routeFetch({
      "GET /api/v1/auth/session": [
        () => json(200, { user, permissions: ["session:self"] }),
      ],
      "GET /api/v1/auth/setup": [() => json(200, { required: false })],
    });
    renderWithClient(<LoginForm />);
    await waitFor(() => expect(replace).toHaveBeenCalledWith("/"));
  });
});
