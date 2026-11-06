import { describe, expect, it } from "vitest";
import {
  formatAge,
  formatBytes,
  formatLsn,
  formatZonedDateTime,
} from "./format";

describe("formatBytes", () => {
  it("uses binary units and keeps bigint exact", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1024)).toBe("1 KiB");
    expect(formatBytes(BigInt(2048))).toBe("2 KiB");
    expect(formatBytes(null)).toBe("—");
    expect(formatBytes(BigInt(2) ** BigInt(60))).toBe("1024 PiB");
  });
});

describe("formatLsn", () => {
  it("renders an unset LSN as a dash", () => {
    expect(formatLsn("")).toBe("—");
    expect(formatLsn("12345")).toBe("12345");
  });
});

describe("formatAge", () => {
  it("counts whole units from now and never goes negative", () => {
    const now = new Date("2026-11-06T12:00:00Z");
    expect(formatAge("2026-11-06T11:00:00Z", now)).toEqual({
      text: "1 小时前",
      stale: false,
    });
    expect(formatAge("2026-11-04T12:00:00Z", now)).toEqual({
      text: "2 天前",
      stale: true,
    });
    expect(formatAge("2026-11-06T12:00:30Z", now).text).toBe("0 秒前");
  });
});

describe("formatZonedDateTime", () => {
  it("names both the policy time zone and UTC", () => {
    const text = formatZonedDateTime("2026-11-06T23:30:00Z", "Asia/Shanghai");
    expect(text).toContain("Asia/Shanghai");
    expect(text).toContain("UTC");
    expect(text).toContain("07:30:00");
    expect(text).toContain("23:30:00");
  });
});
