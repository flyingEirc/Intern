import { describe, expect, it } from "vitest";
import { hasLocalRepository } from "./onboarding";

describe("hasLocalRepository", () => {
  it("is false with no repositories", () => {
    expect(hasLocalRepository([])).toBe(false);
  });

  it("is false when only offsite repositories exist", () => {
    expect(hasLocalRepository([{ kind: "ftp" }, { kind: "ftp" }])).toBe(false);
  });

  it("is true when a local repository exists", () => {
    expect(hasLocalRepository([{ kind: "ftp" }, { kind: "local" }])).toBe(true);
  });
});
