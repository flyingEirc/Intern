import { describe, expect, it } from "vitest";
import { scheduleSummary } from "./policy";

describe("scheduleSummary", () => {
  it("describes both schedules in business wording", () => {
    expect(
      scheduleSummary({
        full_cron: "0 2 * * *",
        incremental_cron: "0 * * * *",
      }),
    ).toEqual({
      text: "每天 02:00 执行全量保护；每小时第 00 分执行增量保护",
      expressions: [],
    });
  });

  it("keeps a custom expression as weak diagnostic text", () => {
    expect(
      scheduleSummary({
        full_cron: "15 3 * * 2",
        incremental_cron: "0 */6 * * *",
      }),
    ).toEqual({
      text: "每周二 03:15 执行全量保护；每 6 小时第 00 分执行增量保护",
      expressions: [],
    });
    expect(
      scheduleSummary({
        full_cron: "0 2 1,15 * *",
        incremental_cron: "0 * * * *",
      }),
    ).toEqual({
      text: "自定义全量安排；每小时第 00 分执行增量保护",
      expressions: ["0 2 1,15 * *"],
    });
  });
});
