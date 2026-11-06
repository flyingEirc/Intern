import { z } from "zod";
import { zUsername } from "./api/generated/zod.gen";

// The backend counts password length in Unicode code points
// (utf8.RuneCountInString), so the form does too instead of UTF-16 units.
const codePoints = (value: string) => [...value].length;

export const loginFormSchema = z.object({
  username: z
    .string()
    .trim()
    .min(1, "请输入用户名")
    .refine(
      (value) => zUsername.safeParse(value).success,
      "用户名只能包含字母、数字、点、下划线和连字符（3–64 位）",
    ),
  password: z.string().min(1, "请输入密码"),
});

export const passwordFormSchema = z
  .object({
    current_password: z.string().min(1, "请输入当前密码"),
    new_password: z
      .string()
      .refine(
        (value) => codePoints(value) >= 12 && codePoints(value) <= 256,
        "新密码需为 12–256 个字符",
      ),
    confirm_password: z.string(),
  })
  .refine((value) => value.new_password === value.confirm_password, {
    path: ["confirm_password"],
    message: "两次输入的新密码不一致",
  });

export type FieldErrors<T> = Partial<Record<keyof T, string>>;

/** First message per field, for FieldError. */
export function fieldErrors<T>(error: z.ZodError<T>): FieldErrors<T> {
  const result: FieldErrors<T> = {};
  for (const issue of error.issues) {
    const key = issue.path[0] as keyof T | undefined;
    if (key !== undefined && result[key] === undefined) {
      result[key] = issue.message;
    }
  }
  return result;
}
