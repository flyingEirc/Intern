import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { replica, sessions } from "@/test/fixtures";
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
  });
}

describe("ReplicasView", () => {
  it("shows the failure and the retry to an operator", async () => {
    routes(sessions.operator);
    renderWithClient(<ReplicasView />);

    expect(await screen.findByText("ftp_refused")).toBeInTheDocument();
    expect(screen.getByText("425")).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "重试" })).toBeInTheDocument();
  });

  it("shows no retry button to a viewer", async () => {
    routes(sessions.viewer);
    renderWithClient(<ReplicasView />);

    expect(await screen.findByText("ftp_refused")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "重试" }),
    ).not.toBeInTheDocument();
  });
});
