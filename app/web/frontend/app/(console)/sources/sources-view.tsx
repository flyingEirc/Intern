"use client";

import { DatabaseIcon, PlusIcon } from "lucide-react";
import Link from "next/link";
import { useMemo, useState } from "react";
import {
  DataPanel,
  LoadMoreButton,
  RowActions,
} from "@/components/resources/data-panel";
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
  const [query, setQuery] = useState("");
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.sources,
  );
  const sources = list.data?.pages.flatMap((page) => page.items) ?? [];
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return sources;
    return sources.filter((source) =>
      [source.name, source.socket, source.server_version ?? ""]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [query, sources]);

  return (
    <div className="flex flex-col gap-5">
      <PageHeader title="数据源" description="管理需要保护的 MySQL 数据库。">
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
        <DataPanel
          title="数据源列表"
          count={visible.length}
          search={query}
          onSearch={setQuery}
          footer={
            <LoadMoreButton
              hasNextPage={list.hasNextPage}
              isFetching={list.isFetchingNextPage}
              onLoadMore={() => list.fetchNextPage()}
            />
          }
        >
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>连接路径</TableHead>
                <TableHead>服务器版本</TableHead>
                <TableHead>密码</TableHead>
                <TableHead>最近预检</TableHead>
                <TableHead>更新时间</TableHead>
                <TableHead>操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={7} className="h-20 text-center">
                    没有匹配的记录
                  </TableCell>
                </TableRow>
              ) : null}
              {visible.map((source) => (
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
                  <TableCell>
                    <RowActions
                      label={source.name}
                      actions={[
                        {
                          label: "查看",
                          href: `/sources/detail?id=${source.id}`,
                        },
                        ...(canManage
                          ? [
                              {
                                label: "编辑",
                                href: `/sources/new?id=${source.id}`,
                              },
                            ]
                          : []),
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
