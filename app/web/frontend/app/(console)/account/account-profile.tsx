"use client";

import { Diagnostics } from "@/components/resources/diagnostics";
import { PageHeader } from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useSession } from "@/hooks/use-session";
import { formatDateTime } from "@/lib/resources/format";
import { roleLabel } from "@/lib/roles";

export function AccountProfile() {
  const { data } = useSession();
  if (!data) return null;
  return (
    <>
      <PageHeader
        title="账户与密码"
        description="查看账号信息并管理登录密码。"
      />
      <Card>
        <CardHeader>
          <CardTitle>当前账号</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2">
            <dt className="text-muted-foreground">用户名</dt>
            <dd className="break-all font-medium">{data.user.username}</dd>
            <dt className="text-muted-foreground">角色</dt>
            <dd>
              <Badge variant="outline">{roleLabel[data.user.role]}</Badge>
            </dd>
            <dt className="text-muted-foreground">创建时间</dt>
            <dd>{formatDateTime(data.user.created_at)}</dd>
          </dl>
        </CardContent>
      </Card>
      <Diagnostics title="有效权限" description="排查账号访问权限时展开。">
        <div className="flex flex-wrap gap-2">
          {data.permissions.map((permission) => (
            <Badge key={permission} variant="secondary" className="font-mono">
              {permission}
            </Badge>
          ))}
        </div>
      </Diagnostics>
    </>
  );
}
