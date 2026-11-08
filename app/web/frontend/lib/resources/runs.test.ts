import { describe, expect, it } from "vitest";
import { isTerminalRun } from "./runs";

describe("isTerminalRun", () => {
  it("treats only completed, failed and canceled as terminal", () => {
    expect(isTerminalRun("completed")).toBe(true);
    expect(isTerminalRun("failed")).toBe(true);
    expect(isTerminalRun("canceled")).toBe(true);
    expect(isTerminalRun("queued")).toBe(false);
    expect(isTerminalRun("running")).toBe(false);
  });
});
