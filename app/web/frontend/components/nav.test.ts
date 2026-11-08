import { describe, expect, it } from "vitest";
import { navItemsFor, navTitle } from "./nav";

describe("console navigation", () => {
  it("titles the resource pages and their query-parameter routes", () => {
    expect(navTitle("/repositories")).toBe("备份仓库");
    expect(navTitle("/repositories/new")).toBe("新建仓库");
    expect(navTitle("/repositories/detail")).toBe("仓库详情");
    expect(navTitle("/sources")).toBe("MySQL 数据源");
    expect(navTitle("/policies/detail")).toBe("策略详情");
    expect(navTitle("/runs")).toBe("运行记录");
    expect(navTitle("/runs/detail")).toBe("运行详情");
    expect(navTitle("/backups/detail")).toBe("备份详情");
    expect(navTitle("/replicas")).toBe("副本复制");
    expect(navTitle("/restores")).toBe("恢复");
    expect(navTitle("/restores/new")).toBe("新建恢复");
    expect(navTitle("/restores/detail")).toBe("恢复详情");
    expect(navTitle("/users")).toBe("用户");
    expect(navTitle("/users/new")).toBe("新建用户");
    expect(navTitle("/users/detail")).toBe("用户详情");
    expect(navTitle("/audit")).toBe("审计日志");
    expect(navTitle("/unknown")).toBe("Intern");
  });

  it("shows the resource entries only to sessions with resources:read", () => {
    const visible = navItemsFor(["session:self", "resources:read"]).map(
      (item) => item.href,
    );
    expect(visible).toContain("/repositories");
    expect(visible).toContain("/sources");
    expect(visible).toContain("/policies");
    expect(visible).toContain("/replicas");
    expect(visible).not.toContain("/runs");
    expect(
      navItemsFor(["session:self"]).map((item) => item.href),
    ).not.toContain("/repositories");
  });

  it("shows the run entry only to sessions with runs:read", () => {
    expect(
      navItemsFor(["session:self", "runs:read"]).map((item) => item.href),
    ).toContain("/runs");
  });

  it("shows the restore entry only to sessions with restores:read", () => {
    expect(
      navItemsFor(["session:self", "restores:read"]).map((item) => item.href),
    ).toContain("/restores");
    expect(
      navItemsFor(["session:self", "resources:read"]).map((item) => item.href),
    ).not.toContain("/restores");
  });

  it("shows users and audit only to the permissions that own them", () => {
    const admin = navItemsFor([
      "session:self",
      "users:manage",
      "audit:read",
    ]).map((item) => item.href);
    expect(admin).toContain("/users");
    expect(admin).toContain("/audit");
    const operator = navItemsFor([
      "session:self",
      "resources:read",
      "runs:read",
    ]).map((item) => item.href);
    expect(operator).not.toContain("/users");
    expect(operator).not.toContain("/audit");
  });
});
