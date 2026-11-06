"use client";

import { CalendarClockIcon, PlusIcon } from "lucide-react";
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
import {
  usePolicyPages,
  useRepositoryChoices,
  useSourceChoices,
} from "@/lib/resources/queries";

export function PoliciesView() {
  const session = useSession();
  const list = usePolicyPages();
  const sources = useSourceChoices();
  const repositories = useRepositoryChoices();
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.policies,
  );
  const policies = list.data?.pages.flatMap((page) => page.items) ?? [];
  const sourceNames = new Map(
    (sources.data?.items ?? []).map((source) => [source.id, source.name]),
  );
  const repositoryNames = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="备份策略"
        description="全量与增量计划、本地仓库与可选 FTP 副本；启用前会运行真实预检。"
      >
        {canManage ? (
          <Button nativeButton={false} render={<Link href="/policies/new" />}>
            <PlusIcon data-icon="inline-start" />
            新建策略
          </Button>
        ) : null}
      </PageHeader>

      {list.isPending ? <ListSkeleton /> : null}
      {list.isError ? (
        <ResourceError error={list.error} onRetry={() => list.refetch()} />
      ) : null}

      {list.isSuccess && policies.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <CalendarClockIcon />
            </EmptyMedia>
            <EmptyTitle>还没有备份策略</EmptyTitle>
            <EmptyDescription>
              策略绑定数据源与本地仓库，可再选一个 FTP
              副本仓库；启用时要求预检通过。
            </EmptyDescription>
            {canManage ? (
              <EmptyContent>
                <Button
                  nativeButton={false}
                  render={<Link href="/policies/new" />}
                >
                  新建策略
                </Button>
              </EmptyContent>
            ) : null}
          </EmptyHeader>
        </Empty>
      ) : null}

      {policies.length > 0 ? (
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>名称</TableHead>
                <TableHead>数据源</TableHead>
                <TableHead>本地仓库</TableHead>
                <TableHead>副本仓库</TableHead>
                <TableHead>全量 / 增量</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>更新时间</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {policies.map((policy) => (
                <TableRow key={policy.id}>
                  <TableCell>
                    <Link
                      className="font-medium underline-offset-4 hover:underline"
                      href={`/policies/detail?id=${policy.id}`}
                    >
                      {policy.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    {sourceNames.get(policy.source_id) ?? policy.source_id}
                  </TableCell>
                  <TableCell>
                    {repositoryNames.get(policy.local_repository_id) ??
                      policy.local_repository_id}
                  </TableCell>
                  <TableCell>
                    {policy.replica_repository_id === null
                      ? "—"
                      : (repositoryNames.get(policy.replica_repository_id) ??
                        policy.replica_repository_id)}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {policy.full_cron} / {policy.incremental_cron}
                  </TableCell>
                  <TableCell>
                    <Badge variant={policy.enabled ? "secondary" : "outline"}>
                      {policy.enabled ? "已启用" : "未启用"}
                    </Badge>
                  </TableCell>
                  <TableCell>{formatDateTime(policy.updated_at)}</TableCell>
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
