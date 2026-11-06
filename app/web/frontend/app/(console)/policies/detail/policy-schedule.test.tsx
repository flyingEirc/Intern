import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { policy, policyId, schedule, sessions } from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { PolicySchedule } from "./policy-schedule";

const push = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace: vi.fn() }),
}));

const keys = [
  "11111111-1111-1111-1111-111111111111",
  "22222222-2222-2222-2222-222222222222",
];

beforeEach(() => {
  let issued = 0;
  vi.stubGlobal("crypto", {
    randomUUID: () => keys[issued++] ?? "extra",
  });
});

afterEach(() => {
  fetchMock.mockReset();
  push.mockReset();
  vi.unstubAllGlobals();
});

function routes(extra: Record<string, Array<() => Response>> = {}) {
  routeFetch({
    "GET /api/v1/auth/session": [() => json(200, sessions.operator())],
    [`GET /api/v1/policies/${policyId}/schedule`]: [
      () => json(200, schedule()),
    ],
    ...extra,
  });
}

function triggerRequests(): Request[] {
  return fetchMock.mock.calls
    .map(([input]) => input as Request)
    .filter((request) => request.method === "POST");
}

describe("PolicySchedule", () => {
  it("renders the missed badge and both time zones", async () => {
    routes();
    renderWithClient(<PolicySchedule policyId={policyId} canTrigger />);

    expect(
      await screen.findByText("已错过，下一次检查即执行"),
    ).toBeInTheDocument();
    const cells = screen.getAllByRole("cell");
    const times = cells
      .map((cell) => cell.textContent ?? "")
      .filter((text) => text.includes("UTC"));
    expect(times).toHaveLength(2);
    for (const time of times) {
      expect(time).toContain("Asia/Shanghai");
      expect(time).toContain("UTC");
    }
  });

  it("notes that a disabled policy's fires are not executed", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.operator())],
      [`GET /api/v1/policies/${policyId}/schedule`]: [
        () => json(200, schedule({ enabled: false })),
      ],
    });
    renderWithClient(<PolicySchedule policyId={policyId} canTrigger />);

    expect(
      await screen.findByText(
        /策略未启用。下面列出的是启用后将会排定的触发时间/,
      ),
    ).toBeInTheDocument();
    void policy;
  });

  it("keeps one idempotency key across the retry and mints a new one per click", async () => {
    routes({
      [`POST /api/v1/policies/${policyId}/runs`]: [
        () =>
          json(500, {
            error: {
              code: "internal",
              message: "internal error",
              request_id: "req-500",
            },
          }),
        () => json(202, { run_id: "run-created" }),
        () => json(202, { run_id: "run-second" }),
      ],
    });
    renderWithClient(<PolicySchedule policyId={policyId} canTrigger />);
    const user = userEvent.setup();
    await screen.findByRole("button", { name: "立即全量" });

    await user.click(screen.getByRole("button", { name: "立即全量" }));
    expect(await screen.findByText("触发失败")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "立即全量" }));
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith("/runs/detail?id=run-created"),
    );

    const full = triggerRequests();
    expect(full).toHaveLength(2);
    expect(full[0]?.headers.get("Idempotency-Key")).toBe(keys[0]);
    expect(full[1]?.headers.get("Idempotency-Key")).toBe(keys[0]);
    expect(await full[0]?.json()).toEqual({ kind: "full" });

    await user.click(screen.getByRole("button", { name: "立即增量" }));
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith("/runs/detail?id=run-second"),
    );
    const incremental = triggerRequests().at(-1);
    expect(incremental?.headers.get("Idempotency-Key")).toBe(keys[1]);
    expect(await incremental?.json()).toEqual({ kind: "incremental" });
  });

  it("sends one key when the button is activated twice before React re-renders", async () => {
    // A real double click: both activations run in one task, before the
    // pending state disables the button. The server's idempotency answers
    // both requests with the same run only if they carry the same key.
    routes({
      [`POST /api/v1/policies/${policyId}/runs`]: [
        () => json(202, { run_id: "run-once" }),
        () => json(202, { run_id: "run-once" }),
      ],
    });
    renderWithClient(<PolicySchedule policyId={policyId} canTrigger />);
    const button = await screen.findByRole("button", { name: "立即全量" });

    // One batch, as in the browser: no render happens between the clicks.
    act(() => {
      button.click();
      button.click();
    });

    await waitFor(() =>
      expect(push).toHaveBeenCalledWith("/runs/detail?id=run-once"),
    );
    const sent = triggerRequests().map((request) =>
      request.headers.get("Idempotency-Key"),
    );
    expect(sent.length).toBeGreaterThan(0);
    expect(new Set(sent)).toEqual(new Set([keys[0]]));
  });

  it("hides the trigger buttons from a viewer", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.viewer())],
      [`GET /api/v1/policies/${policyId}/schedule`]: [
        () => json(200, schedule()),
      ],
    });
    renderWithClient(<PolicySchedule policyId={policyId} canTrigger={false} />);

    expect(
      await screen.findByText("当前账号只有查看权限，不能手动触发运行。"),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "立即全量" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "立即增量" }),
    ).not.toBeInTheDocument();
  });
});
