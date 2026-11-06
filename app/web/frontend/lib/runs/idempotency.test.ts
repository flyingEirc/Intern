import { describe, expect, it, vi } from "vitest";
import { triggerAttempt } from "./idempotency";

describe("triggerAttempt", () => {
  it("reuses the key of the click in flight and mints one per new click", () => {
    const freshKey = vi
      .fn<() => string>()
      .mockReturnValueOnce("key-1")
      .mockReturnValueOnce("key-2");

    const first = triggerAttempt(null, "full", freshKey);
    const retry = triggerAttempt(first, "full", freshKey);
    const again = triggerAttempt(null, "full", freshKey);

    expect(retry).toBe(first);
    expect(again).toEqual({ key: "key-2", kind: "full" });
    expect(first.key).not.toBe(again.key);
    expect(freshKey).toHaveBeenCalledTimes(2);
  });

  it("does not reuse a key across kinds", () => {
    const freshKey = vi.fn<() => string>().mockReturnValue("key-2");
    const next = triggerAttempt(
      { key: "key-1", kind: "full" },
      "incremental",
      freshKey,
    );
    expect(next).toEqual({ key: "key-2", kind: "incremental" });
  });
});
