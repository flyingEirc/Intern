"use client";

import { useQuery } from "@tanstack/react-query";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useSession } from "@/hooks/use-session";
import { readinessOptions } from "@/lib/api/generated/@tanstack/react-query.gen";
import { describeError } from "@/lib/api/messages";
import { roleLabel } from "@/lib/roles";

const dateTime = new Intl.DateTimeFormat("zh-CN", {
  dateStyle: "medium",
  timeStyle: "short",
});

function Readiness() {
  const readiness = useQuery({
    ...readinessOptions(),
    refetchInterval: 30_000,
    retry: false,
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle>服务状态</CardTitle>
        <CardDescription>元数据库连接与迁移版本（/readyz）</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-2">
        {readiness.isPending ? (
          <Skeleton className="h-6 w-20" />
        ) : readiness.isError ? (
          <>
            <Badge variant="destructive">未就绪</Badge>
            <p className="text-sm text-muted-foreground">
              {describeError(readiness.error)}
            </p>
          </>
        ) : (
          <Badge variant="secondary">就绪</Badge>
        )}
      </CardContent>
    </Card>
  );
}

export function Overview() {
  const { data } = useSession();
  if (!data) {
    return null;
  }
  const { user, permissions } = data;
  return (
    <div className="grid gap-6 md:grid-cols-2 xl:grid-cols-3">
      <Card>
        <CardHeader>
          <CardTitle>当前用户</CardTitle>
          <CardDescription>本次会话登录的账号</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">用户名</dt>
            <dd className="font-medium">{user.username}</dd>
            <dt className="text-muted-foreground">角色</dt>
            <dd>
              <Badge variant="outline">{roleLabel[user.role]}</Badge>
            </dd>
            <dt className="text-muted-foreground">创建时间</dt>
            <dd>{dateTime.format(new Date(user.created_at))}</dd>
          </dl>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>有效权限</CardTitle>
          <CardDescription>由角色决定，服务端逐请求校验</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-2">
          {permissions.map((permission) => (
            <Badge key={permission} variant="secondary" className="font-mono">
              {permission}
            </Badge>
          ))}
        </CardContent>
      </Card>
      <Readiness />
    </div>
  );
}
