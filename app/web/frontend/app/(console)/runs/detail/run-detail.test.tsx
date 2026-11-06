import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { run, runId, runLog, sessions } from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { RunDetail } from "./run-detail";

const replace = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace, push: vi.fn() }),
  useSearchParams: () => new URLSearchParams({ id: runId }),
}));

afterEach(() => {
  fetchMock.mockReset();
  replace.mockReset();
});

function routes(body: ReturnType<typeof run>) {
  routeFetch({
    "GET /api/v1/auth/session": [() => json(200, sessions.viewer())],
    "GET /api/v1/policies": [() => json(200, { items: [], next_before: null })],
    "GET /api/v1/sources": [() => json(200, { items: [], next_before: null })],
    [`GET /api/v1/runs/${runId}`]: [() => json(200, body)],
    [`GET /api/v1/runs/${runId}/logs`]: [
      () =>
        json(200, { items: [runLog()], next_after: null, truncated: false }),
    ],
  });
}

describe("RunDetail permissions", () => {
  it("shows no cancel button to a viewer", async () => {
    routes(run());
    renderWithClient(<RunDetail />);

    expect(await screen.findByText("运行中")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "取消运行" }),
    ).not.toBeInTheDocument();
  });

  it("shows the recorded cancel request", async () => {
    routes(run({ cancel_requested: true }));
    renderWithClient(<RunDetail />);

    expect(await screen.findByText("已请求取消")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "取消运行" }),
    ).not.toBeInTheDocument();
  });
});

void userEvent;
