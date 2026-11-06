import {
  CalendarClockIcon,
  CopyIcon,
  DatabaseIcon,
  HardDriveIcon,
  HistoryIcon,
  KeyRoundIcon,
  LayoutDashboardIcon,
  ListChecksIcon,
  type LucideIcon,
} from "lucide-react";
import type { Permission } from "@/lib/api/generated/types.gen";

export type NavItem = {
  href: string;
  title: string;
  icon: LucideIcon;
  permission?: Permission;
};

export const navItems: NavItem[] = [
  { href: "/", title: "概览", icon: LayoutDashboardIcon },
  {
    href: "/repositories",
    title: "备份仓库",
    icon: HardDriveIcon,
    permission: "resources:read",
  },
  {
    href: "/sources",
    title: "MySQL 数据源",
    icon: DatabaseIcon,
    permission: "resources:read",
  },
  {
    href: "/policies",
    title: "备份策略",
    icon: CalendarClockIcon,
    permission: "resources:read",
  },
  {
    href: "/runs",
    title: "运行记录",
    icon: ListChecksIcon,
    permission: "runs:read",
  },
  {
    href: "/replicas",
    title: "副本复制",
    icon: CopyIcon,
    permission: "resources:read",
  },
  {
    href: "/restores",
    title: "恢复",
    icon: HistoryIcon,
    permission: "restores:read",
  },
  { href: "/account", title: "账户与密码", icon: KeyRoundIcon },
];

/** Sidebar entries the session may see; the server still authorises each call. */
export function navItemsFor(
  permissions: readonly Permission[] | undefined,
): NavItem[] {
  return navItems.filter(
    (item) =>
      item.permission === undefined ||
      (permissions?.includes(item.permission) ?? false),
  );
}

const pageTitles: Record<string, string> = {
  "/repositories/new": "新建仓库",
  "/repositories/detail": "仓库详情",
  "/sources/new": "新建数据源",
  "/sources/detail": "数据源详情",
  "/policies/new": "新建策略",
  "/policies/detail": "策略详情",
  "/runs/detail": "运行详情",
  "/backups/detail": "备份详情",
  "/restores/new": "新建恢复",
  "/restores/detail": "恢复详情",
};

/**
 * Page heading for the console header. Detail and new pages are query-parameter
 * routes, so they have their own titles; everything else uses the longest
 * sidebar prefix that matches.
 */
export function navTitle(pathname: string): string {
  const exact = pageTitles[pathname];
  if (exact !== undefined) {
    return exact;
  }
  const matches = navItems
    .filter(
      (item) => pathname === item.href || pathname.startsWith(`${item.href}/`),
    )
    .sort((left, right) => right.href.length - left.href.length);
  return matches[0]?.title ?? "Intern";
}
