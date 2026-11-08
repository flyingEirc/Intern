import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import {
  fetchMock,
  json,
  renderWithClient,
  routeFetch,
  user,
} from "@/test/render";
import { RepositoriesView } from "./repositories-view";

afterEach(() => {
  fetchMock.mockReset();
});

describe("RepositoriesView", () => {
  it("shows no write actions to a viewer", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [
        () =>
          json(200, {
            user: { ...user, role: "viewer" },
            permissions: ["session:self", "resources:read"],
          }),
      ],
      "GET /api/v1/repositories": [
        () => json(200, { items: [], next_before: null }),
      ],
    });
    renderWithClient(<RepositoriesView />);

    expect(await screen.findByText("还没有备份仓库")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "新建仓库" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "新建仓库" }),
    ).not.toBeInTheDocument();
  });
});
