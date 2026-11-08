import { screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import {
  chain,
  protection,
  replicaRepositoryId,
  sessions,
  sourceId,
} from "@/test/fixtures";
import { fetchMock, json, renderWithClient, routeFetch } from "@/test/render";
import { SourceProtection } from "./source-protection";

afterEach(() => {
  fetchMock.mockReset();
});

describe("SourceProtection", () => {
  it("renders the local point and each remote repository separately", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.viewer())],
      "GET /api/v1/repositories": [
        () => json(200, { items: [], next_before: null }),
      ],
      [`GET /api/v1/sources/${sourceId}/chains`]: [
        () => json(200, { items: [chain()] }),
      ],
      [`GET /api/v1/sources/${sourceId}/protection`]: [
        () => json(200, protection()),
      ],
    });
    renderWithClient(<SourceProtection sourceId={sourceId} />);

    const rows = await screen.findAllByRole("row");
    const local = rows.find((row) => row.textContent?.startsWith("本地"));
    const remote = rows.find((row) => row.textContent?.startsWith("异地"));
    expect(local?.textContent).toContain("2026");
    expect(remote?.textContent).toContain(replicaRepositoryId);
    // Two days against one: the remote point is older and stays its own row.
    expect(local?.textContent).not.toEqual(remote?.textContent);
    expect(screen.queryByText("尚无保护点")).not.toBeInTheDocument();
    expect(screen.getByText("查看副本状态")).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "恢复到此备份" }),
    ).not.toBeInTheDocument();
  });

  it("shows an absent point as absent instead of borrowing the other side", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.viewer())],
      "GET /api/v1/repositories": [
        () => json(200, { items: [], next_before: null }),
      ],
      [`GET /api/v1/sources/${sourceId}/chains`]: [
        () => json(200, { items: [] }),
      ],
      [`GET /api/v1/sources/${sourceId}/protection`]: [
        () => json(200, protection({ local: null, remote: [] })),
      ],
    });
    renderWithClient(<SourceProtection sourceId={sourceId} />);

    expect(await screen.findByText("尚无保护点")).toBeInTheDocument();
    expect(
      screen.getByText("还没有异地保护点：没有任何副本仓库完成过校验。"),
    ).toBeInTheDocument();
  });

  it("offers a restore link on a completed chain member when planning is allowed", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [() => json(200, sessions.operator())],
      "GET /api/v1/repositories": [
        () => json(200, { items: [], next_before: null }),
      ],
      [`GET /api/v1/sources/${sourceId}/chains`]: [
        () => json(200, { items: [chain()] }),
      ],
      [`GET /api/v1/sources/${sourceId}/protection`]: [
        () => json(200, protection()),
      ],
    });
    renderWithClient(<SourceProtection sourceId={sourceId} canPlanRestore />);

    const href = `/restores/new?backup_id=${chain().members[0]?.id}`;
    await waitFor(() => {
      expect(document.querySelector(`a[href="${href}"]`)).not.toBeNull();
    });
    expect(document.querySelector(`a[href="${href}"]`)).toHaveTextContent(
      "恢复到此备份",
    );
  });
});
