import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { RepositoryForm } from "./repository-form";

const push = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

afterEach(() => {
  fetchMock.mockReset();
  push.mockReset();
});

const repository = {
  id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b",
  name: "ftp-backup",
  kind: "ftp" as const,
  tls_mode: "explicit" as const,
  ftp: {
    host: "127.0.0.1",
    port: 21,
    base_path: "/backups",
    username: "backup",
  },
  has_secret: true,
  plaintext_credentials: false,
  created_at: "2026-11-06T08:00:00Z",
  updated_at: "2026-11-06T08:00:00Z",
};

async function fillFtp() {
  const u = userEvent.setup();
  await u.click(screen.getByRole("radio", { name: "FTP / FTPS" }));
  await u.type(screen.getByLabelText("名称"), "ftp-backup");
  await u.type(screen.getByLabelText("主机"), "127.0.0.1");
  await u.clear(screen.getByLabelText("端口"));
  await u.type(screen.getByLabelText("端口"), "21");
  await u.type(screen.getByLabelText("基础路径"), "/backups");
  await u.type(screen.getByLabelText("用户名"), "backup");
  await u.type(screen.getByLabelText("密码"), "repo-password");
}

describe("RepositoryForm", () => {
  it("requires an explicit FTP TLS choice and sends the selected mode", async () => {
    routeFetch({
      "POST /api/v1/repositories": [() => json(201, repository)],
    });
    renderWithClient(<RepositoryForm />);
    await fillFtp();

    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "创建仓库" }));
    expect(
      await screen.findByText("请选择 FTP 传输方式：明文、显式 TLS 或隐式 TLS"),
    ).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();

    await userEvent
      .setup()
      .click(screen.getByRole("radio", { name: "明文 FTP" }));
    expect(screen.getByText("凭据与数据明文传输")).toBeInTheDocument();

    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "创建仓库" }));
    const request = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((item) => item.method === "POST");
    expect(await request?.json()).toEqual({
      name: "ftp-backup",
      kind: "ftp",
      tls_mode: "plain",
      ftp: {
        host: "127.0.0.1",
        port: 21,
        base_path: "/backups",
        username: "backup",
      },
      password: "repo-password",
    });
    expect(push).toHaveBeenCalledWith(
      "/repositories/detail?id=0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b",
    );
  }, 20000);

  it("never renders a stored password and omits a blank one from PUT", async () => {
    routeFetch({
      [`PUT /api/v1/repositories/${repository.id}`]: [
        () => json(200, repository),
      ],
    });
    renderWithClient(<RepositoryForm repository={repository} />);

    const password = screen.getByLabelText("密码");
    expect(password).toHaveValue("");
    expect(password).toHaveAttribute("placeholder", "已设置（留空保持不变）");
    expect(screen.getByText("显式 FTPS（创建后不可更改）")).toBeInTheDocument();

    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "保存修改" }));
    const request = fetchMock.mock.calls
      .map(([input]) => input as Request)
      .find((item) => item.method === "PUT");
    const body = await request?.json();
    expect(body).not.toHaveProperty("password");
    expect(body).toMatchObject({
      name: "ftp-backup",
      kind: "ftp",
      tls_mode: "explicit",
    });
  });
});
