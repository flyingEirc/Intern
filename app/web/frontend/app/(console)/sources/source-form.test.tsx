import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { SourceForm } from "./source-form";

const push = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push }),
}));

afterEach(() => {
  fetchMock.mockReset();
  push.mockReset();
});

async function fill() {
  const u = userEvent.setup();
  await u.type(screen.getByLabelText("名称"), "primary");
  await u.type(screen.getByLabelText("Socket"), "/run/mysqld/mysqld.sock");
  await u.type(screen.getByLabelText("Datadir"), "/var/lib/mysql");
  await u.type(screen.getByLabelText("用户名"), "backup");
  await u.type(screen.getByLabelText("密码"), "source-password");
}

describe("SourceForm", () => {
  it("explains an unavailable deployment with the fixed text and backend message", async () => {
    routeFetch({
      "POST /api/v1/sources": [
        () =>
          json(503, {
            error: {
              code: "unavailable",
              message: "This operation is not available in this deployment",
              request_id: "req-503",
            },
          }),
      ],
    });
    renderWithClient(<SourceForm />);
    await fill();
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "创建数据源" }));

    expect(
      await screen.findByText(/当前部署缺少完成此操作所需的配置/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/This operation is not available in this deployment/),
    ).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });
});
