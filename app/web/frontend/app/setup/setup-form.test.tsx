import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchMock,
  json,
  renderWithClient,
  routeFetch,
  user,
} from "@/test/render";
import { SetupForm } from "./setup-form";

const replace = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
}));

afterEach(() => {
  fetchMock.mockReset();
  replace.mockReset();
});

describe("SetupForm", () => {
  it("creates the first administrator through the generated SDK", async () => {
    routeFetch({
      "GET /api/v1/auth/setup": [() => json(200, { required: true })],
      "POST /api/v1/auth/setup": [() => json(201, { user })],
    });
    renderWithClient(<SetupForm />);

    const u = userEvent.setup();
    await u.type(await screen.findByLabelText("管理员用户名"), "first-admin");
    await u.type(screen.getByLabelText("设置密码"), "correct-horse-battery");
    await u.type(screen.getByLabelText("确认密码"), "correct-horse-battery");
    await u.click(
      screen.getByRole("button", { name: "创建管理员并进入控制台" }),
    );

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/"));
    const request = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((item) => item.method === "POST");
    expect(await request?.json()).toEqual({
      username: "first-admin",
      password: "correct-horse-battery",
    });
    expect(screen.queryByDisplayValue("correct-horse-battery")).toBeNull();
  });

  it("validates both password fields before sending anything", async () => {
    routeFetch({
      "GET /api/v1/auth/setup": [() => json(200, { required: true })],
    });
    renderWithClient(<SetupForm />);

    const u = userEvent.setup();
    await u.type(await screen.findByLabelText("管理员用户名"), "first-admin");
    await u.type(screen.getByLabelText("设置密码"), "short");
    await u.type(screen.getByLabelText("确认密码"), "different");
    await u.click(
      screen.getByRole("button", { name: "创建管理员并进入控制台" }),
    );

    expect(
      await screen.findByText("密码需为 12–256 个字符"),
    ).toBeInTheDocument();
    expect(
      fetchMock.mock.calls.some(
        ([input]) => (input as Request).method === "POST",
      ),
    ).toBe(false);
  });

  it("reports a completed setup without offering a second anonymous creation", async () => {
    routeFetch({
      "GET /api/v1/auth/setup": [() => json(200, { required: false })],
    });
    renderWithClient(<SetupForm />);
    expect(await screen.findByText("初始化已完成")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "前往登录" })).toBeInTheDocument();
  });
});
