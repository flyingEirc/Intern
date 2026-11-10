"use client";

import { PlusIcon, UsersIcon } from "lucide-react";
import Link from "next/link";
import { useMemo, useState } from "react";
import { DataPanel, RowActions } from "@/components/resources/data-panel";
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
  const [query, setQuery] = useState("");

  const users = list.data?.items ?? [];
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return users;
    return users.filter((account) =>
      [account.username, roleLabel[account.role]]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [query, users]);
  if (!canManage) {
    return <NoPermission action="管理用户" note="用户管理仅对管理员开放。" />;
  }

  return (
    <div className="flex flex-col gap-5">
      <PageHeader title="用户" description="管理管理员、操作员与只读账号。">
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
        <DataPanel
          title="用户列表"
          count={visible.length}
          search={query}
          onSearch={setQuery}
        >
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>用户名</TableHead>
                <TableHead>角色</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>创建时间</TableHead>
                <TableHead>更新时间</TableHead>
                <TableHead>操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={6} className="h-20 text-center">
                    没有匹配的记录
                  </TableCell>
                </TableRow>
              ) : null}
              {visible.map((account) => (
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
                  <TableCell>
                    <RowActions
                      label={account.username}
                      actions={[
                        {
                          label: "管理",
                          href: `/users/detail?id=${account.id}`,
                        },
                      ]}
                    />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </DataPanel>
      ) : null}
    </div>
  );
}
