import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { policy, policyId, schedule, sessions } from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { PolicyDetail } from "./policy-detail";

const search = new URLSearchParams([["id", policyId]]);

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => search,
}));

afterEach(() => {
  fetchMock.mockReset();
});

function routes(throttle: number) {
  routeFetch({
    "GET /api/v1/auth/session": [() => json(200, sessions.operator())],
    [`GET /api/v1/policies/${policyId}`]: [
      () => json(200, policy({ throttle })),
    ],
    "GET /api/v1/sources": [() => json(200, { items: [], next_before: null })],
    "GET /api/v1/repositories": [
      () => json(200, { items: [], next_before: null }),
    ],
    [`GET /api/v1/policies/${policyId}/schedule`]: [
      () => json(200, schedule()),
    ],
  });
}

async function openTuning() {
  await userEvent.click(
    await screen.findByRole("button", { name: "系统管理的备份参数" }),
  );
}

describe("PolicyDetail throttle", () => {
  it("shows no extra limit when throttle is zero", async () => {
    routes(0);
    renderWithClient(<PolicyDetail />);
    await openTuning();
    expect(await screen.findByText("不额外限速")).toBeInTheDocument();
  });

  it("names a positive throttle as blocks per second", async () => {
    routes(1);
    renderWithClient(<PolicyDetail />);
    await openTuning();
    expect(await screen.findByText("每秒 1 块")).toBeInTheDocument();
    expect(screen.queryByText(/字节\/秒/)).not.toBeInTheDocument();
  });
});
