"use client";

import {
  ListSkeleton,
  NoPermission,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useSession } from "@/hooks/use-session";
import type { AuditLog } from "@/lib/api/generated/types.gen";
import {
  actorTypeLabel,
  auditActionLabel,
  auditDetailJson,
  auditIdNumber,
  auditResultLabel,
  formatAuditDetail,
} from "@/lib/resources/audit";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import { useAuditLogPages } from "@/lib/resources/queries";

const resultVariant: Record<
  AuditLog["result"],
  "secondary" | "outline" | "destructive"
> = {
  success: "secondary",
  denied: "outline",
  failed: "destructive",
};

export function AuditView() {
  const session = useSession();
  const canRead = hasPermission(session.data?.permissions, "audit:read");
  const pages = useAuditLogPages(canRead);

  if (!canRead) {
    return (
      <NoPermission
        action="读取审计日志"
        note="当前账号没有读取审计日志的权限。审计记录只对管理员开放，服务端仍会逐请求拒绝。"
      />
    );
  }

  const entries = pages.data?.pages.flatMap((page) => page.items) ?? [];
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="审计日志"
        description="按时间倒序。详情是结构化字段，不含密码或凭据。"
      />
      {pages.isPending ? <ListSkeleton /> : null}
      {pages.isError ? (
        <ResourceError error={pages.error} onRetry={() => pages.refetch()} />
      ) : null}
      {pages.isSuccess && entries.length === 0 ? (
        <p className="text-sm text-muted-foreground">还没有审计记录。</p>
      ) : null}
      {entries.length > 0 ? (
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>时间</TableHead>
                <TableHead>操作者</TableHead>
                <TableHead>动作</TableHead>
                <TableHead>资源</TableHead>
                <TableHead>结果</TableHead>
                <TableHead>状态码</TableHead>
                <TableHead>IP</TableHead>
                <TableHead>请求 ID</TableHead>
                <TableHead>详情</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {entries.map((entry) => (
                <TableRow key={auditIdNumber(entry.id)}>
                  <TableCell className="whitespace-nowrap">
                    {formatDateTime(entry.ts)}
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col gap-1">
                      <Badge variant="outline">
                        {actorTypeLabel[entry.actor_type]}
                      </Badge>
                      <span>{entry.actor_name || "—"}</span>
                    </div>
                  </TableCell>
                  <TableCell>
                    {auditActionLabel[entry.action] ?? entry.action}
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-col">
                      <span>{entry.resource_type || "—"}</span>
                      <span className="font-mono text-xs text-muted-foreground">
                        {entry.resource_id || "—"}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell>
                    <Badge variant={resultVariant[entry.result]}>
                      {auditResultLabel[entry.result]}
                    </Badge>
                  </TableCell>
                  <TableCell>{entry.status ?? "—"}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {entry.ip || "—"}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {entry.request_id || "—"}
                  </TableCell>
                  <TableCell>
                    <details>
                      <summary className="cursor-pointer">
                        {formatAuditDetail(entry.detail)}
                      </summary>
                      <pre className="mt-2 max-w-sm overflow-x-auto font-mono text-xs whitespace-pre-wrap">
                        {auditDetailJson(entry.detail)}
                      </pre>
                    </details>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {pages.hasNextPage ? (
            <div>
              <Button
                variant="outline"
                disabled={pages.isFetchingNextPage}
                onClick={() => pages.fetchNextPage()}
              >
                {pages.isFetchingNextPage ? (
                  <Spinner data-icon="inline-start" />
                ) : null}
                加载更多
              </Button>
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
