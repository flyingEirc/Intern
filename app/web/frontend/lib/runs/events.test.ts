import { describe, expect, it } from "vitest";
import { interpretRunFrame, sequenceNumber } from "./events";

const log = {
  seq: 7,
  ts: "2026-11-06T08:00:00Z",
  level: "info",
  event: "started",
  message: "backup started",
  data: null,
};

describe("interpretRunFrame", () => {
  it("accepts a log row, plain-text end, and a dataless revoked", () => {
    expect(interpretRunFrame("message", log)).toEqual({
      type: "log",
      event: log,
    });
    expect(interpretRunFrame("end", "completed")).toEqual({ type: "end" });
    expect(interpretRunFrame("revoked", undefined)).toEqual({
      type: "revoked",
    });
  });

  it("ignores heartbeat comments and rows the contract does not describe", () => {
    expect(interpretRunFrame(undefined, undefined)).toEqual({ type: "ignore" });
    expect(interpretRunFrame("message", { seq: 0 })).toEqual({
      type: "ignore",
    });
  });
});

describe("sequenceNumber", () => {
  it("narrows a contract bigint sequence to a number", () => {
    expect(sequenceNumber(BigInt(4))).toBe(4);
    expect(() => sequenceNumber(0)).toThrow();
  });
});
