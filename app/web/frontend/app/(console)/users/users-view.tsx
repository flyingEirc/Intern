"use client";

import { PlusIcon, UsersIcon } from "lucide-react";
import Link from "next/link";
import {
  ListSkeleton,
  NoPermission,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useSession } from "@/hooks/use-session";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import { useUsers } from "@/lib/resources/queries";
import { roleLabel } from "@/lib/roles";

export function UsersView() {
  const session = useSession();
  const canManage = hasPermission(session.data?.permissions, "users:manage");
  const list = useUsers(canManage);

  if (!canManage) {
    return (
      <NoPermission
        action="管理用户"
        note="当前账号没有管理用户的权限。用户列表只对管理员开放，服务端仍会逐请求拒绝。"
      />
    );
  }

  const users = list.data?.items ?? [];
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="用户"
        description="管理员、操作员与只读账号。密码只在创建和重置时写入，之后不再显示。"
      >
        <Button nativeButton={false} render={<Link href="/users/new" />}>
          <PlusIcon data-icon="inline-start" />
          新建用户
        </Button>
      </PageHeader>

      {list.isPending ? <ListSkeleton /> : null}
      {list.isError ? (
        <ResourceError error={list.error} onRetry={() => list.refetch()} />
      ) : null}

      {list.isSuccess && users.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <UsersIcon />
            </EmptyMedia>
            <EmptyTitle>还没有用户</EmptyTitle>
            <EmptyDescription>
              创建一个操作员或只读账号后，对方可以用初始密码登录。
            </EmptyDescription>
            <EmptyContent>
              <Button nativeButton={false} render={<Link href="/users/new" />}>
                新建用户
              </Button>
            </EmptyContent>
          </EmptyHeader>
        </Empty>
      ) : null}

      {users.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>用户名</TableHead>
              <TableHead>角色</TableHead>
              <TableHead>状态</TableHead>
              <TableHead>创建时间</TableHead>
              <TableHead>更新时间</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {users.map((account) => (
              <TableRow key={account.id}>
                <TableCell>
                  <Link
                    className="font-medium underline-offset-4 hover:underline"
                    href={`/users/detail?id=${account.id}`}
                  >
                    {account.username}
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant="outline">{roleLabel[account.role]}</Badge>
                </TableCell>
                <TableCell>
                  <Badge
                    variant={account.disabled ? "destructive" : "secondary"}
                  >
                    {account.disabled ? "已禁用" : "已启用"}
                  </Badge>
                </TableCell>
                <TableCell>{formatDateTime(account.created_at)}</TableCell>
                <TableCell>{formatDateTime(account.updated_at)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : null}
    </div>
  );
}
