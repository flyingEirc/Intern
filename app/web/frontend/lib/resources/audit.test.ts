import { describe, expect, it } from "vitest";
import type { AuditLog } from "@/lib/api/generated/types.gen";
import { auditDetailJson, auditIdNumber, formatAuditDetail } from "./audit";

const detail: AuditLog["detail"] = {
  role: "operator",
  password_changed: true,
  disabled: false,
};

describe("auditIdNumber", () => {
  it("narrows a contract bigint cursor and refuses an unsafe one", () => {
    expect(auditIdNumber(BigInt(41))).toBe(41);
    expect(() =>
      auditIdNumber(BigInt(Number.MAX_SAFE_INTEGER) + BigInt(2)),
    ).toThrow(/safe integer/);
  });
});

describe("audit detail", () => {
  it("renders the closed detail without ever including a secret field", () => {
    expect(formatAuditDetail(detail)).toBe(
      "角色：操作员，密码已更改：是，禁用：否",
    );
    expect(formatAuditDetail({})).toBe("—");
    const json = auditDetailJson(detail);
    expect(json).toContain('"role": "operator"');
    expect(json).not.toContain("password_hash");
    expect(json).not.toContain("correct-horse");
  });
});
