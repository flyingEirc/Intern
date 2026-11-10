import { describe, expect, it } from "vitest";
import { isTerminalRun, runEventText, runFailureReason } from "./runs";

describe("isTerminalRun", () => {
  it("treats only completed, failed and canceled as terminal", () => {
    expect(isTerminalRun("completed")).toBe(true);
    expect(isTerminalRun("failed")).toBe(true);
    expect(isTerminalRun("canceled")).toBe(true);
    expect(isTerminalRun("queued")).toBe(false);
    expect(isTerminalRun("running")).toBe(false);
  });
});

describe("runEventText", () => {
  it("maps known engine events to Chinese and keeps unknown ones generic", () => {
    expect(runEventText("preflight_failed")).toBe("环境检查未通过");
    expect(runEventText("replication_enqueued")).toBe("已加入异地复制");
    expect(runEventText("future_event")).toBe("运行事件");
  });
});

describe("runFailureReason", () => {
  it("explains known failures and never returns an empty reason", () => {
    expect(runFailureReason("requires_full", null)).toContain("全量备份");
    expect(runFailureReason("backup_failed", "x")).toContain("没有生成");
    expect(runFailureReason(null, null)).toContain("技术诊断");
    expect(runFailureReason("future_class", null)).toContain("技术诊断");
  });
});
