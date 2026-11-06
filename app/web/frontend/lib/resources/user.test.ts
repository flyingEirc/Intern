import { describe, expect, it } from "vitest";
import { ApiError } from "@/lib/api/errors";
import {
  isLastAdminConflict,
  lastAdminExplanation,
  lastAdminMessage,
  newUserFormSchema,
  passwordResetSchema,
} from "./user";

describe("newUserFormSchema", () => {
  const valid = {
    username: "operator.one",
    password: "correct-horse",
    role: "operator" as const,
  };

  it("accepts the contract username pattern and a 12-character password", () => {
    expect(newUserFormSchema.safeParse(valid).success).toBe(true);
    expect(
      newUserFormSchema.safeParse({ ...valid, username: "  op_1  " }).data
        ?.username,
    ).toBe("op_1");
  });

  it("rejects a username outside the contract pattern before any request", () => {
    for (const username of ["ab", "has space", "名用户", "a".repeat(65)]) {
      const parsed = newUserFormSchema.safeParse({ ...valid, username });
      expect(parsed.success, username).toBe(false);
    }
  });

  it("counts the password in Unicode code points, 12 through 256", () => {
    const eleven = "😀".repeat(11);
    expect(
      newUserFormSchema.safeParse({ ...valid, password: eleven }).success,
    ).toBe(false);
    expect(
      newUserFormSchema.safeParse({ ...valid, password: "😀".repeat(12) })
        .success,
    ).toBe(true);
    expect(
      newUserFormSchema.safeParse({ ...valid, password: "a".repeat(257) })
        .success,
    ).toBe(false);
    expect(
      newUserFormSchema.safeParse({ ...valid, password: "a".repeat(256) })
        .success,
    ).toBe(true);
  });
});

describe("passwordResetSchema", () => {
  it("treats an empty reset as no change and rejects a short one", () => {
    expect(passwordResetSchema.safeParse({ password: "" }).success).toBe(true);
    expect(
      passwordResetSchema.safeParse({ password: "too-short" }).success,
    ).toBe(false);
    expect(
      passwordResetSchema.safeParse({ password: "long-enough-pw" }).success,
    ).toBe(true);
  });
});

describe("isLastAdminConflict", () => {
  it("matches only the backend's exact last-administrator message", () => {
    const conflict = (message: string) =>
      new ApiError({
        status: 409,
        code: "conflict",
        message,
        requestId: "req-409",
      });
    expect(isLastAdminConflict(conflict(lastAdminMessage))).toBe(true);
    expect(lastAdminExplanation).toContain("最后一个启用的管理员");
    expect(isLastAdminConflict(conflict("Username already exists"))).toBe(
      false,
    );
    expect(
      isLastAdminConflict(
        new ApiError({
          status: 409,
          code: "preflight_failed",
          message: lastAdminMessage,
        }),
      ),
    ).toBe(false);
  });
});
