import { describe, expect, it } from "vitest";
import { ApiError } from "@/lib/api/errors";
import {
  describeRestoreError,
  exportStartCommand,
  maxVerifyStatementBytes,
  maxVerifyStatements,
  parseVerifySql,
  refusalText,
  shortenDigest,
  sideEffectStatements,
} from "./restore";

const digest = "ab".repeat(32);

describe("restore plan text", () => {
  it("shortens a digest without hiding either end", () => {
    expect(shortenDigest(digest)).toBe("abababababab…abababababab");
    expect(shortenDigest("abc")).toBe("abc");
  });

  it("states each terminal side effect without calling it a rollback", () => {
    const present = sideEffectStatements({
      work_dir_removed: false,
      pending_export_removed: false,
      export_published: true,
    });
    expect(present).toEqual([
      "工作目录未删除",
      "待发布导出未删除",
      "导出已发布",
    ]);
    const absent = sideEffectStatements({
      work_dir_removed: true,
      pending_export_removed: true,
      export_published: false,
    }).join(" ");
    expect(absent).toContain("工作目录已删除");
    expect(absent).toContain("导出未发布");
    expect(absent).not.toContain("回滚");
  });

  it("explains a known refusal and keeps an unknown code readable", () => {
    expect(refusalText("chain_broken")).toContain("备份链不完整");
    expect(refusalText("brand_new_code")).toContain("未知");
    expect(refusalText(undefined)).toContain("没有给出机器码");
  });
});

describe("parseVerifySql", () => {
  it("accepts one SELECT per line, including a trailing semicolon", () => {
    const parsed = parseVerifySql("SELECT 1\n  select * from t;  \n\n");
    expect(parsed).toEqual({
      ok: true,
      statements: ["SELECT 1", "select * from t;"],
    });
  });

  it("accepts an empty editor as no statements", () => {
    expect(parseVerifySql("   \n")).toEqual({ ok: true, statements: [] });
  });

  it("rejects a 17th statement before anything is submitted", () => {
    const text = Array.from(
      { length: maxVerifyStatements + 1 },
      () => "SELECT 1",
    ).join("\n");
    const parsed = parseVerifySql(text);
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) {
      expect(parsed.message).toContain("16");
      expect(parsed.message).toContain("17");
    }
  });

  it("rejects one statement longer than 4096 bytes", () => {
    const text = `SELECT ${"x".repeat(maxVerifyStatementBytes)}`;
    const parsed = parseVerifySql(text);
    expect(parsed.ok).toBe(false);
    if (!parsed.ok) {
      expect(parsed.message).toContain("4096");
      expect(parsed.message).toContain("第 1 条");
    }
  });

  it("rejects a statement that is not one read-only SELECT", () => {
    expect(parseVerifySql("DROP TABLE t").ok).toBe(false);
    expect(parseVerifySql("SELECT 1; SELECT 2").ok).toBe(false);
    expect(parseVerifySql("SELECTX 1").ok).toBe(false);
  });

  it("accepts the longest single statement the engine allows", () => {
    const text = `SELECT ${"x".repeat(maxVerifyStatementBytes - "SELECT ".length)}`;
    expect(new TextEncoder().encode(text).length).toBe(maxVerifyStatementBytes);
    expect(parseVerifySql(text).ok).toBe(true);
  });
});

describe("describeRestoreError", () => {
  it("names the machine code, its explanation and the backend message", () => {
    const error = new ApiError({
      status: 409,
      code: "restore_refused",
      message: "Restore refused",
      detailCode: "lsn_discontinuous",
    });
    const text = describeRestoreError(error);
    expect(text).toContain("lsn_discontinuous");
    expect(text).toContain("LSN");
    expect(text).toContain("Restore refused");
  });

  it("states the Retry-After seconds of a 429", () => {
    const error = new ApiError({
      status: 429,
      code: "rate_limited",
      message: "Too many attempts",
      retryAfter: 17,
    });
    expect(describeRestoreError(error)).toContain("17 秒");
  });
});

describe("exportStartCommand", () => {
  it("uses the documented container recipe and the restore id", () => {
    const command = exportStartCommand("abc");
    expect(command).toContain(
      "docker run --rm --platform linux/amd64 -u 999:999",
    );
    expect(command).toContain("--datadir=/exports/abc/data");
    expect(command).toContain("--skip-log-bin");
    expect(command).toContain("--innodb-buffer-pool-size=64M");
    expect(command).toContain('"$INTERN_MYSQL_IMAGE"');
  });
});
