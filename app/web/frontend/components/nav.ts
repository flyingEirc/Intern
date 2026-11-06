import {
  KeyRoundIcon,
  LayoutDashboardIcon,
  type LucideIcon,
} from "lucide-react";

export type NavItem = { href: string; title: string; icon: LucideIcon };

export const navItems: NavItem[] = [
  { href: "/", title: "概览", icon: LayoutDashboardIcon },
  { href: "/account", title: "账户与密码", icon: KeyRoundIcon },
];

export function navTitle(pathname: string): string {
  return navItems.find((item) => item.href === pathname)?.title ?? "Intern";
}
