"use client";

import { HardDriveIcon, PlusIcon } from "lucide-react";
import Link from "next/link";
import { PlaintextBadge } from "@/components/resources/plaintext-warning";
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
import { useRepositoryPages } from "@/lib/resources/queries";
import {
  repositoryEndpoint,
  repositoryKindLabel,
  tlsModeLabel,
} from "@/lib/resources/repository";

export function RepositoriesView() {
  const session = useSession();
  const list = useRepositoryPages();
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.repositories,
  );
  const repositories = list.data?.pages.flatMap((page) => page.items) ?? [];

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="备份仓库"
        description="本地目录、明文 FTP 与 FTPS 仓库；凭据只写不读，写入性探测按仓库配置执行。"
      >
        {canManage ? (
          <Button
            nativeButton={false}
            render={<Link href="/repositories/new" />}
          >
            <PlusIcon data-icon="inline-start" />
            新建仓库
          </Button>
        ) : null}
      </PageHeader>

      {list.isPending ? <ListSkeleton /> : null}
      {list.isError ? (
        <ResourceError error={list.error} onRetry={() => list.refetch()} />
      ) : null}

      {list.isSuccess && repositories.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <HardDriveIcon />
            </EmptyMedia>
            <EmptyTitle>还没有备份仓库</EmptyTitle>
            <EmptyDescription>
              备份需要至少一个本地仓库；副本仓库可以是明文 FTP 或 FTPS。
            </EmptyDescription>
            {canManage ? (
              <EmptyContent>
                <Button
                  nativeButton={false}
                  render={<Link href="/repositories/new" />}
                >
                  新建仓库
                </Button>
              </EmptyContent>
            ) : null}
          </EmptyHeader>
        </Empty>
      ) : null}

      {repositories.length > 0 ? (
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>类型</TableHead>
                <TableHead>传输方式</TableHead>
                <TableHead>位置</TableHead>
                <TableHead>凭据</TableHead>
                <TableHead>更新时间</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {repositories.map((repository) => (
                <TableRow key={repository.id}>
                  <TableCell>
                    <Link
                      className="font-medium underline-offset-4 hover:underline"
                      href={`/repositories/detail?id=${repository.id}`}
                    >
                      {repository.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline">
                      {repositoryKindLabel[repository.kind]}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-2">
                      <span>{tlsModeLabel[repository.tls_mode]}</span>
                      {repository.plaintext_credentials ? (
                        <PlaintextBadge />
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {repositoryEndpoint(repository)}
                  </TableCell>
                  <TableCell>
                    <Badge variant="secondary">
                      {repository.has_secret ? "已设置" : "未设置"}
                    </Badge>
                  </TableCell>
                  <TableCell>{formatDateTime(repository.updated_at)}</TableCell>
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
