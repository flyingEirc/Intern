import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Restore } from "@/lib/api/generated/types.gen";
import { restore, restoreId, restoreLog, sessions } from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { RestoreDetail } from "./restore-detail";

const search = new URLSearchParams([["id", restoreId]]);

vi.mock("next/navigation", () => ({
  useSearchParams: () => search,
}));

afterEach(() => {
  fetchMock.mockReset();
});

function routes(body: Restore, session = sessions.admin()) {
  routeFetch({
    "GET /api/v1/auth/session": [() => json(200, session)],
    "GET /api/v1/repositories": [
      () => json(200, { items: [], next_before: null }),
    ],
    [`GET /api/v1/restores/${restoreId}`]: [() => json(200, body)],
    [`GET /api/v1/restores/${restoreId}/logs`]: [
      () =>
        json(200, {
          items: [
            restoreLog(),
            restoreLog({
              seq: 2,
              message: "restore phase preparing",
              data: { phase: "preparing" },
            }),
          ],
          next_after: null,
          truncated: false,
        }),
    ],
  });
}

describe("RestoreDetail side effects", () => {
  it.each([
    ["completed", true, true, true],
    ["failed", true, true, false],
    ["canceled", true, true, false],
  ] as const)("states the three effects of a %s restore without calling it a rollback", async (status, work, pending, published) => {
    routes(
      restore({
        status,
        export_path: published
          ? `/var/lib/intern/export/${restoreId}/data`
          : null,
        export_bytes: published ? 8192 : null,
        side_effects: {
          work_dir_removed: work,
          pending_export_removed: pending,
          export_published: published,
        },
      }),
    );
    renderWithClient(<RestoreDetail />);

    expect(await screen.findByText("终态副作用")).toBeInTheDocument();
    const expected = [
      work ? "工作目录已删除" : "工作目录未删除",
      pending ? "待发布导出已删除" : "待发布导出未删除",
      published ? "导出已发布" : "导出未发布",
    ];
    for (const statement of expected) {
      expect(screen.getByText(statement)).toBeInTheDocument();
    }
    expect(document.body.textContent).not.toContain("回滚");
  });

  it("shows the documented container command once an export was published", async () => {
    routes(restore());
    renderWithClient(<RestoreDetail />);

    const command = await screen.findByText(/docker run --rm/);
    expect(command.textContent).toContain(
      `--datadir=/exports/${restoreId}/data`,
    );
    expect(command.textContent).toContain("--skip-log-bin");
    expect(command.textContent).toContain("--innodb-buffer-pool-size=64M");
    expect(
      await screen.findByText("restore phase extracting"),
    ).toBeInTheDocument();
    expect(document.body.textContent).toMatch(/已记录：解包\s*→\s*prepare/);
  });

  it("hides cancellation from an operator", async () => {
    routes(
      restore({ status: "running", phase: "extracting" }),
      sessions.operator(),
    );
    renderWithClient(<RestoreDetail />);

    expect(await screen.findByText("执行中")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "取消恢复" }),
    ).not.toBeInTheDocument();
  });

  it("reads the log once more when the restore reaches a terminal state", async () => {
    // The completed row arrives after the log poll of the same tick, so the
    // terminal entries only show if the page reads the log after the change.
    let completed = false;
    const running = {
      items: [restoreLog()],
      next_after: null,
      truncated: false,
    };
    const finished = {
      items: [
        restoreLog(),
        restoreLog({ seq: 2, message: "restore completed", data: {} }),
      ],
      next_after: null,
      truncated: false,
    };
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.admin())],
      "GET /api/v1/repositories": [
        () => json(200, { items: [], next_before: null }),
      ],
      [`GET /api/v1/restores/${restoreId}`]: [
        () => json(200, restore({ status: "running", phase: "preparing" })),
        async () => {
          await new Promise((resolve) => setTimeout(resolve, 300));
          completed = true;
          return json(200, restore());
        },
      ],
      [`GET /api/v1/restores/${restoreId}/logs`]: Array.from(
        { length: 4 },
        () => () => json(200, completed ? finished : running),
      ),
    });
    renderWithClient(<RestoreDetail />);

    expect(
      await screen.findByText("restore completed", {}, { timeout: 5000 }),
    ).toBeInTheDocument();
  }, 10_000);
});
