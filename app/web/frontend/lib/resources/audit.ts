import type { AuditLog } from "@/lib/api/generated/types.gen";
import { roleLabel } from "@/lib/roles";

/**
 * Narrows a contract audit ID or `before` cursor to the number the generated
 * query parameter accepts. Audit history is not bounded the way a run log is,
 * so a value past the safe integer range is refused rather than rounded.
 */
export function auditIdNumber(value: bigint | number): number {
  const number = typeof value === "bigint" ? Number(value) : value;
  if (!Number.isSafeInteger(number) || number < 1) {
    throw new Error("audit ID is outside the safe integer range");
  }
  return number;
}

export const actorTypeLabel: Record<AuditLog["actor_type"], string> = {
  user: "用户",
  system: "系统",
  anonymous: "匿名",
};

export const auditResultLabel: Record<AuditLog["result"], string> = {
  success: "成功",
  denied: "拒绝",
  failed: "失败",
};

/**
 * Chinese labels for the audit actions the backend writes. An action the
 * contract does not enumerate falls back to its raw name, so a new backend
 * action stays readable instead of disappearing.
 */
export const auditActionLabel: Record<string, string> = {
  "auth.login": "登录",
  "auth.setup": "首次管理员初始化",
  "auth.logout": "退出登录",
  "auth.password": "修改自己的密码",
  "users.create": "创建用户",
  "users.create_admin": "创建管理员",
  "users.update": "修改用户",
  "users.revoke_sessions": "吊销会话",
};

const detailKeyLabel: Record<string, string> = {
  role: "角色",
  disabled: "禁用",
  password_changed: "密码已更改",
  method: "方法",
  route: "路径",
  plaintext_ftp: "明文 FTP",
};

/** Stable, readable text of the contract's closed detail object. */
export function formatAuditDetail(detail: AuditLog["detail"]): string {
  const parts: string[] = [];
  for (const [key, value] of Object.entries(detail)) {
    if (value === undefined) {
      continue;
    }
    const label = detailKeyLabel[key] ?? key;
    let rendered: string;
    if (typeof value === "boolean") {
      rendered = value ? "是" : "否";
    } else if (
      key === "role" &&
      (value === "admin" || value === "operator" || value === "viewer")
    ) {
      rendered = roleLabel[value];
    } else {
      rendered = value;
    }
    parts.push(`${label}：${rendered}`);
  }
  return parts.length === 0 ? "—" : parts.join("，");
}

/**
 * The detail object rendered as JSON text. `JSON.stringify` never interprets
 * the value as markup, and the contract's detail carries no secret.
 */
export function auditDetailJson(detail: AuditLog["detail"]): string {
  return JSON.stringify(detail, null, 2);
}
