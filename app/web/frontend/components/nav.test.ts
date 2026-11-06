import { describe, expect, it } from "vitest";
import { navItemsFor, navTitle } from "./nav";

describe("console navigation", () => {
  it("titles the resource pages and their query-parameter routes", () => {
    expect(navTitle("/repositories")).toBe("备份仓库");
    expect(navTitle("/repositories/new")).toBe("新建仓库");
    expect(navTitle("/repositories/detail")).toBe("仓库详情");
    expect(navTitle("/sources")).toBe("MySQL 数据源");
    expect(navTitle("/policies/detail")).toBe("策略详情");
    expect(navTitle("/unknown")).toBe("Intern");
  });

  it("shows the resource entries only to sessions with resources:read", () => {
    const visible = navItemsFor(["session:self", "resources:read"]).map(
      (item) => item.href,
    );
    expect(visible).toContain("/repositories");
    expect(visible).toContain("/sources");
    expect(visible).toContain("/policies");
    expect(
      navItemsFor(["session:self"]).map((item) => item.href),
    ).not.toContain("/repositories");
  });
});
