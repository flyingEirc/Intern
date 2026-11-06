"use client";

import { DatabaseIcon, PlusIcon } from "lucide-react";
import Link from "next/link";
import {
  ListSkeleton,
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
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import { useSourcePages } from "@/lib/resources/queries";

function PreflightBadge({ preflight }: { preflight: { ok: boolean } | null }) {
  if (preflight === null) {
    return <Badge variant="outline">未预检</Badge>;
  }
  return (
    <Badge variant={preflight.ok ? "secondary" : "destructive"}>
      {preflight.ok ? "通过" : "未通过"}
    </Badge>
  );
}

export function SourcesView() {
  const session = useSession();
  const list = useSourcePages();
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.sources,
  );
  const sources = list.data?.pages.flatMap((page) => page.items) ?? [];

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="MySQL 数据源"
        description="通过 socket 与 datadir 读取的 MySQL 实例；密码只写不读，预检会记录服务器身份。"
      >
        {canManage ? (
          <Button nativeButton={false} render={<Link href="/sources/new" />}>
            <PlusIcon data-icon="inline-start" />
            新建数据源
          </Button>
        ) : null}
      </PageHeader>

      {list.isPending ? <ListSkeleton /> : null}
      {list.isError ? (
        <ResourceError error={list.error} onRetry={() => list.refetch()} />
      ) : null}

      {list.isSuccess && sources.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <DatabaseIcon />
            </EmptyMedia>
            <EmptyTitle>还没有数据源</EmptyTitle>
            <EmptyDescription>
              数据源描述 intern 容器内可见的 MySQL socket 与
              datadir；预检需要至少一个本地仓库。
            </EmptyDescription>
            {canManage ? (
              <EmptyContent>
                <Button
                  nativeButton={false}
                  render={<Link href="/sources/new" />}
                >
                  新建数据源
                </Button>
              </EmptyContent>
            ) : null}
          </EmptyHeader>
        </Empty>
      ) : null}

      {sources.length > 0 ? (
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>Socket</TableHead>
                <TableHead>服务器版本</TableHead>
                <TableHead>密码</TableHead>
                <TableHead>最近预检</TableHead>
                <TableHead>更新时间</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {sources.map((source) => (
                <TableRow key={source.id}>
                  <TableCell>
                    <Link
                      className="font-medium underline-offset-4 hover:underline"
                      href={`/sources/detail?id=${source.id}`}
                    >
                      {source.name}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {source.socket}
                  </TableCell>
                  <TableCell>{source.server_version ?? "—"}</TableCell>
                  <TableCell>
                    <Badge variant="secondary">
                      {source.has_secret ? "已设置" : "未设置"}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <PreflightBadge preflight={source.last_preflight} />
                  </TableCell>
                  <TableCell>{formatDateTime(source.updated_at)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {list.hasNextPage ? (
            <div>
              <Button
                variant="outline"
                disabled={list.isFetchingNextPage}
                onClick={() => list.fetchNextPage()}
              >
                {list.isFetchingNextPage ? (
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
