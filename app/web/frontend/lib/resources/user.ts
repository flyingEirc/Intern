import { z } from "zod";
import { ApiError } from "@/lib/api/errors";
import type { Role } from "@/lib/api/generated/types.gen";
import { zUsername } from "@/lib/api/generated/zod.gen";
import { roleLabel } from "@/lib/roles";

// The backend counts password length in Unicode code points
// (utf8.RuneCountInString), so the form does too instead of UTF-16 units.
const codePoints = (value: string) => [...value].length;

const passwordRule = z
  .string()
  .refine(
    (value) => codePoints(value) >= 12 && codePoints(value) <= 256,
    "密码需为 12–256 个字符",
  );

const usernameRule = z
  .string()
  .trim()
  .min(1, "请输入用户名")
  .refine(
    (value) => zUsername.safeParse(value).success,
    "用户名只能包含字母、数字、点、下划线和连字符（3–64 位）",
  );

export const roles = [
  "admin",
  "operator",
  "viewer",
] as const satisfies readonly Role[];

export const roleSelectItems: Record<Role, string> = roleLabel;

export const newUserFormSchema = z.object({
  username: usernameRule,
  password: passwordRule,
  role: z.enum(roles, { error: "请选择角色" }),
});

export type NewUserFormValues = z.infer<typeof newUserFormSchema>;

export function newUserFormValues(): NewUserFormValues {
  return { username: "", password: "", role: "operator" };
}

/** A reset is optional; an empty field means "leave the password unchanged". */
export const passwordResetSchema = z.object({
  password: z.union([z.literal(""), passwordRule]),
});

/**
 * Fixed explanation for the last-enabled-administrator guard. The backend
 * answers 409 conflict with one exact message (auth resourceError); a
 * username collision uses the same status and code with a different message,
 * so the text is matched exactly and never guessed from the status alone.
 */
export const lastAdminMessage =
  "Cannot disable or demote the last enabled administrator";

export const lastAdminExplanation =
  "这是最后一个启用的管理员，不能被禁用或降级。请先创建并启用另一位管理员。";

export function isLastAdminConflict(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    error.status === 409 &&
    error.code === "conflict" &&
    error.message === lastAdminMessage
  );
}
