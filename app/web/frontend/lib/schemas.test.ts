import { describe, expect, it } from "vitest";
import { fieldErrors, loginFormSchema, passwordFormSchema } from "./schemas";

describe("passwordFormSchema", () => {
  const valid = {
    current_password: "old-password",
    new_password: "correct-horse-battery",
    confirm_password: "correct-horse-battery",
  };

  it("counts Unicode code points like the backend, not UTF-16 units", () => {
    // 11 emoji are 22 UTF-16 units but only 11 characters: too short.
    const eleven = "😀".repeat(11);
    const short = passwordFormSchema.safeParse({
      ...valid,
      new_password: eleven,
      confirm_password: eleven,
    });
    expect(short.success).toBe(false);

    const twelve = "😀".repeat(12);
    expect(
      passwordFormSchema.safeParse({
        ...valid,
        new_password: twelve,
        confirm_password: twelve,
      }).success,
    ).toBe(true);
  });

  it("enforces the 256-character upper bound", () => {
    const long = "a".repeat(257);
    const result = passwordFormSchema.safeParse({
      ...valid,
      new_password: long,
      confirm_password: long,
    });
    expect(result.success).toBe(false);
    expect(
      passwordFormSchema.safeParse({
        ...valid,
        new_password: "a".repeat(256),
        confirm_password: "a".repeat(256),
      }).success,
    ).toBe(true);
  });

  it("reports a mismatched confirmation on the confirmation field", () => {
    const result = passwordFormSchema.safeParse({
      ...valid,
      confirm_password: "something-else-entirely",
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      expect(fieldErrors(result.error)).toEqual({
        confirm_password: "两次输入的新密码不一致",
      });
    }
  });
});

describe("loginFormSchema", () => {
  it("applies the contract username pattern after trimming", () => {
    expect(
      loginFormSchema.safeParse({ username: "  admin  ", password: "p" }).data,
    ).toEqual({ username: "admin", password: "p" });
    const invalid = loginFormSchema.safeParse({
      username: "a b",
      password: "p",
    });
    expect(invalid.success).toBe(false);
  });
});
