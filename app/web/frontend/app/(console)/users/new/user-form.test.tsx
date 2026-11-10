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
import { UserForm } from "./user-form";

const push = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

const created = {
  ...user,
  id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a10",
  username: "reader.one",
  role: "viewer" as const,
};

afterEach(() => {
  fetchMock.mockReset();
  push.mockReset();
});

async function submit(username: string, password: string) {
  const actor = userEvent.setup();
  await actor.type(await screen.findByLabelText("用户名"), username);
  await actor.type(screen.getByLabelText("初始密码"), password);
  await actor.click(screen.getByRole("combobox", { name: "角色" }));
  await actor.click(await screen.findByRole("option", { name: "只读" }));
  await actor.click(screen.getByRole("button", { name: "创建用户" }));
}

describe("UserForm", () => {
  it("refuses a username outside the contract pattern without a request", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
    });
    renderWithClient(<UserForm />);
    await submit("has space", "correct-horse-1");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "用户名只能包含字母、数字、点、下划线和连字符（3–64 位）",
    );
    expect(fetchMock).toHaveBeenCalledTimes(1);
  }, 20000);

  it("refuses a password shorter than 12 characters without a request", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
    });
    renderWithClient(<UserForm />);
    await submit("reader.one", "short-pass");

    expect(
      await screen.findByText("密码需为 12–256 个字符"),
    ).toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("sends the password once and clears it after the user is created", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      "POST /api/v1/users": [() => json(201, created)],
      "GET /api/v1/users": [() => json(200, { items: [created] })],
      "GET /api/v1/users/0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a10": [
        () => json(200, created),
      ],
    });
    renderWithClient(<UserForm />);
    await submit("reader.one", "correct-horse-1");

    await waitFor(() =>
      expect(push).toHaveBeenCalledWith(
        "/users/detail?id=0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a10",
      ),
    );
    const post = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((request) => request.method === "POST");
    expect(await post?.text()).toContain('"password":"correct-horse-1"');
    expect(screen.getByLabelText("初始密码")).toHaveValue("");
    expect(
      screen.queryByDisplayValue("correct-horse-1"),
    ).not.toBeInTheDocument();
  });

  it("shows the no-permission state to a session that cannot manage users", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.operator())],
    });
    renderWithClient(<UserForm />);

    expect(await screen.findByText("没有权限")).toBeInTheDocument();
    expect(screen.queryByLabelText("初始密码")).not.toBeInTheDocument();
  });
});
