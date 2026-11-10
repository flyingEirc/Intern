import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { backup, backupId, replica, sessions } from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { ReplicasView } from "./replicas-view";

afterEach(() => {
  fetchMock.mockReset();
});

function routes(session: () => unknown) {
  routeFetch({
    "GET /api/v1/auth/session": [() => json(200, session())],
    "GET /api/v1/repositories": [
      () => json(200, { items: [], next_before: null }),
    ],
    "GET /api/v1/replicas": [() => json(200, { items: [replica()] })],
    [`GET /api/v1/backups/${backupId}`]: [() => json(200, backup())],
  });
}

describe("ReplicasView", () => {
  it("keeps failure details behind one expansion and retries a failed copy", async () => {
    routes(sessions.operator);
    const user = userEvent.setup();
    renderWithClient(<ReplicasView />);
    expect(await screen.findByText(/物理全量/)).toBeInTheDocument();

    expect(screen.queryByText("ftp_refused")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "查看失败详情" }));
    expect(await screen.findByText("ftp_refused")).toBeInTheDocument();
    expect(screen.getByText("425")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });

  it("shows failure details but no retry button to a viewer", async () => {
    routes(sessions.viewer);
    const user = userEvent.setup();
    renderWithClient(<ReplicasView />);

    await user.click(
      await screen.findByRole("button", { name: "查看失败详情" }),
    );
    expect(await screen.findByText("ftp_refused")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "重试" }),
    ).not.toBeInTheDocument();
  });
});
