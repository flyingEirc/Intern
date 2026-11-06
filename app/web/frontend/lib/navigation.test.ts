import { describe, expect, it } from "vitest";
import { loginPath, safeNextPath } from "./navigation";

describe("safeNextPath", () => {
  it.each([
    ["/account", "/account"],
    ["/account?tab=password#top", "/account?tab=password#top"],
    ["/runs/detail?id=42", "/runs/detail?id=42"],
  ])("keeps the same-origin path %s", (raw, expected) => {
    expect(safeNextPath(raw)).toBe(expected);
  });

  it.each([
    [null],
    [""],
    ["account"],
    ["https://evil.example/"],
    ["//evil.example/"],
    ["/\\evil.example/"],
    ["/\t/evil.example/"],
    ["/\n/evil.example"],
    ["/login"],
    ["/login?next=/account"],
  ])("falls back to / for %j", (raw) => {
    expect(safeNextPath(raw)).toBe("/");
  });
});

describe("loginPath", () => {
  it("omits the default destination and carries the reason", () => {
    expect(loginPath("/")).toBe("/login");
    expect(loginPath("/", "signed-out")).toBe("/login?reason=signed-out");
    expect(loginPath("/account?x=1", "expired")).toBe(
      "/login?next=%2Faccount%3Fx%3D1&reason=expired",
    );
  });
});
