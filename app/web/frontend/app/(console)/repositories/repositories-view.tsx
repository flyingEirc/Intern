"use client";

import { HardDriveIcon, PlusIcon } from "lucide-react";
import Link from "next/link";
import { useMemo, useState } from "react";
import {
  DataPanel,
  LoadMoreButton,
  RowActions,
} from "@/components/resources/data-panel";
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
  const [query, setQuery] = useState("");
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.repositories,
  );
  const repositories = list.data?.pages.flatMap((page) => page.items) ?? [];
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return repositories;
    return repositories.filter((repository) =>
      [
        repository.name,
        repositoryKindLabel[repository.kind],
        tlsModeLabel[repository.tls_mode],
        repositoryEndpoint(repository),
      ]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [query, repositories]);

  return (
    <div className="flex flex-col gap-5">
      <PageHeader title="备份仓库" description="管理备份的本地存储和异地副本。">
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
              备份需要至少一个本地仓库；异地副本可以是明文 FTP 或 FTPS。
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
        <DataPanel
          title="仓库列表"
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
                <TableHead>类型</TableHead>
                <TableHead>传输方式</TableHead>
                <TableHead>位置</TableHead>
                <TableHead>凭据</TableHead>
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
              {visible.map((repository) => (
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
                  <TableCell>
                    <RowActions
                      label={repository.name}
                      actions={[
                        {
                          label: "查看",
                          href: `/repositories/detail?id=${repository.id}`,
                        },
                        ...(canManage
                          ? [
                              {
                                label: "编辑",
                                href: `/repositories/new?id=${repository.id}`,
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
