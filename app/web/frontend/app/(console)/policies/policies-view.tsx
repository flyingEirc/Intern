"use client";

import { CalendarClockIcon, PlusIcon } from "lucide-react";
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
import { scheduleSummary } from "@/lib/resources/policy";
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
  const [query, setQuery] = useState("");
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
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return policies;
    return policies.filter((policy) =>
      [
        policy.name,
        sourceNames.get(policy.source_id) ?? "",
        repositoryNames.get(policy.local_repository_id) ?? "",
        scheduleSummary(policy).text,
        ...scheduleSummary(policy).expressions,
      ]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [policies, query, sourceNames, repositoryNames]);

  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        title="保护策略"
        description="安排数据库的全量、增量备份和异地保护。"
      >
        {canManage ? (
          <Button nativeButton={false} render={<Link href="/policies/new" />}>
            <PlusIcon data-icon="inline-start" />
            新建保护策略
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
            <EmptyTitle>还没有保护策略</EmptyTitle>
            <EmptyDescription>
              策略绑定数据源与本地仓库，可再选一个异地副本；启用时要求预检通过。
            </EmptyDescription>
            {canManage ? (
              <EmptyContent>
                <Button
                  nativeButton={false}
                  render={<Link href="/policies/new" />}
                >
                  新建保护策略
                </Button>
              </EmptyContent>
            ) : null}
          </EmptyHeader>
        </Empty>
      ) : null}

      {policies.length > 0 ? (
        <DataPanel
          title="策略列表"
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
                <TableHead>数据源</TableHead>
                <TableHead>本地仓库</TableHead>
                <TableHead>异地副本</TableHead>
                <TableHead>时间安排</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>更新时间</TableHead>
                <TableHead>操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={8} className="h-20 text-center">
                    没有匹配的记录
                  </TableCell>
                </TableRow>
              ) : null}
              {visible.map((policy) => {
                const schedule = scheduleSummary(policy);
                return (
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
                    <TableCell>
                      <span className="flex flex-col gap-1">
                        <span>{schedule.text}</span>
                        {schedule.expressions.map((expression) => (
                          <span
                            key={expression}
                            className="font-mono text-xs text-muted-foreground break-all"
                          >
                            {expression}
                          </span>
                        ))}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Badge variant={policy.enabled ? "secondary" : "outline"}>
                        {policy.enabled ? "已启用" : "未启用"}
                      </Badge>
                    </TableCell>
                    <TableCell>{formatDateTime(policy.updated_at)}</TableCell>
                    <TableCell>
                      <RowActions
                        label={policy.name}
                        actions={[
                          {
                            label: "查看",
                            href: `/policies/detail?id=${policy.id}`,
                          },
                          ...(canManage
                            ? [
                                {
                                  label: "编辑",
                                  href: `/policies/new?id=${policy.id}`,
                                },
                              ]
                            : []),
                        ]}
                      />
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </DataPanel>
      ) : null}
    </div>
  );
}
