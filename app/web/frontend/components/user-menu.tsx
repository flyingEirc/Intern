"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ChevronsUpDownIcon, KeyRoundIcon, LogOutIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { SidebarMenuButton } from "@/components/ui/sidebar";
import { toast } from "@/components/ui/toast";
import { isUnauthorized } from "@/lib/api/errors";
import { logoutMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import type { User } from "@/lib/api/generated/types.gen";
import { describeError } from "@/lib/api/messages";
import { loginPath } from "@/lib/navigation";
import { forgetPrincipal } from "@/lib/principal";
import { roleLabel } from "@/lib/roles";

export function UserMenu({ user }: { user: User }) {
  const router = useRouter();
  const queryClient = useQueryClient();

  const signOut = () => {
    forgetPrincipal(queryClient);
    router.replace(loginPath("/", "signed-out"));
  };

  const logout = useMutation({
    ...logoutMutation(),
    // 401 means the session is already gone, which is what logout wants.
    meta: { handlesUnauthorized: true },
    onSuccess: signOut,
    onError: (error) => {
      if (isUnauthorized(error)) {
        signOut();
        return;
      }
      toast.add({
        type: "error",
        title: "退出登录失败",
        description: describeError(error),
      });
    },
  });

  const initials = user.username.slice(0, 2).toUpperCase();

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<SidebarMenuButton size="lg" aria-label="账户菜单" />}
      >
        <Avatar className="size-8 rounded-lg">
          <AvatarFallback className="rounded-lg">{initials}</AvatarFallback>
        </Avatar>
        <div className="grid flex-1 text-left text-sm leading-tight">
          <span className="truncate font-medium">{user.username}</span>
          <span className="truncate text-xs text-muted-foreground">
            {roleLabel[user.role]}
          </span>
        </div>
        <ChevronsUpDownIcon className="ml-auto" />
      </DropdownMenuTrigger>
      <DropdownMenuContent side="top" align="start" className="min-w-56">
        <DropdownMenuGroup>
          <DropdownMenuLabel>
            {user.username} · {roleLabel[user.role]}
          </DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          <DropdownMenuItem render={<Link href="/account" />}>
            <KeyRoundIcon />
            修改密码
          </DropdownMenuItem>
          <DropdownMenuItem
            variant="destructive"
            disabled={logout.isPending}
            onClick={() => logout.mutate({})}
          >
            <LogOutIcon />
            退出登录
          </DropdownMenuItem>
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
