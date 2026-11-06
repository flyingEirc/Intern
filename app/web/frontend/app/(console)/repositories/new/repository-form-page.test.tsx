import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  fetchMock,
  json,
  renderWithClient,
  routeFetch,
  user,
} from "@/test/render";
import { RepositoryFormPage } from "./repository-form-page";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));

afterEach(() => {
  fetchMock.mockReset();
});

describe("RepositoryFormPage", () => {
  it("shows a no-permission empty state when the session cannot manage repositories", async () => {
    routeFetch({
      "GET /api/v1/auth/session": [
        () =>
          json(200, {
            user: { ...user, role: "viewer" },
            permissions: ["session:self", "resources:read"],
          }),
      ],
    });
    renderWithClient(<RepositoryFormPage />);

    expect(await screen.findByText("没有权限")).toBeInTheDocument();
    expect(screen.queryByLabelText("名称")).not.toBeInTheDocument();
  });
});
