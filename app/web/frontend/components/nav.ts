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
  ScrollTextIcon,
  UsersIcon,
} from "lucide-react";
import type { Permission } from "@/lib/api/generated/types.gen";

export type NavItem = {
  href: string;
  title: string;
  icon: LucideIcon;
  permission?: Permission;
};

export type NavGroup = {
  label: string;
  items: NavItem[];
};

export const navGroups: NavGroup[] = [
  {
    label: "监控",
    items: [
      { href: "/", title: "概览", icon: LayoutDashboardIcon },
      {
        href: "/runs",
        title: "运行记录",
        icon: ListChecksIcon,
        permission: "runs:read",
      },
    ],
  },
  {
    label: "保护",
    items: [
      {
        href: "/sources",
        title: "数据源",
        icon: DatabaseIcon,
        permission: "resources:read",
      },
      {
        href: "/policies",
        title: "保护策略",
        icon: CalendarClockIcon,
        permission: "resources:read",
      },
    ],
  },
  {
    label: "存储",
    items: [
      {
        href: "/repositories",
        title: "备份仓库",
        icon: HardDriveIcon,
        permission: "resources:read",
      },
      {
        href: "/replicas",
        title: "异地副本",
        icon: CopyIcon,
        permission: "resources:read",
      },
    ],
  },
  {
    label: "恢复",
    items: [
      {
        href: "/restores",
        title: "恢复",
        icon: HistoryIcon,
        permission: "restores:read",
      },
    ],
  },
  {
    label: "管理",
    items: [
      {
        href: "/users",
        title: "用户",
        icon: UsersIcon,
        permission: "users:manage",
      },
      {
        href: "/audit",
        title: "审计日志",
        icon: ScrollTextIcon,
        permission: "audit:read",
      },
      {
        href: "/account",
        title: "账户与密码",
        icon: KeyRoundIcon,
      },
    ],
  },
];

export const navItems: NavItem[] = navGroups.flatMap((group) => group.items);

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

/** Grouped entries, with empty groups dropped for the current principal. */
export function navGroupsFor(
  permissions: readonly Permission[] | undefined,
): NavGroup[] {
  return navGroups
    .map((group) => ({
      label: group.label,
      items: group.items.filter(
        (item) =>
          item.permission === undefined ||
          (permissions?.includes(item.permission) ?? false),
      ),
    }))
    .filter((group) => group.items.length > 0);
}

const pageTitles: Record<string, string> = {
  "/repositories/new": "新建仓库",
  "/repositories/detail": "仓库详情",
  "/sources/new": "新建数据源",
  "/sources/detail": "数据源详情",
  "/policies/new": "新建保护策略",
  "/policies/detail": "保护策略详情",
  "/runs/detail": "运行详情",
  "/backups/detail": "备份详情",
  "/restores/new": "新建恢复",
  "/restores/detail": "恢复详情",
  "/users/new": "新建用户",
  "/users/detail": "用户详情",
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
