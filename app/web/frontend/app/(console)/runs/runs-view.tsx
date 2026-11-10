"use client";

import { ListChecksIcon } from "lucide-react";
import Link from "next/link";
import { useMemo, useState } from "react";
import { DataPanel, LoadMoreButton } from "@/components/resources/data-panel";
import {
  ListSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import {
  runStatusTone,
  StatusBadge,
} from "@/components/resources/status-badge";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Field, FieldLabel } from "@/components/ui/field";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { RunStatus } from "@/lib/api/generated/types.gen";
import { formatDateTime } from "@/lib/resources/format";
import { usePolicyChoices, useRunPages } from "@/lib/resources/queries";
import {
  runKindLabel,
  runStatuses,
  runStatusLabel,
  runTriggerLabel,
} from "@/lib/resources/runs";

const anyStatus = "any";
const anyPolicy = "any";

const statusLabels: Record<string, string> = {
  [anyStatus]: "全部状态",
  ...runStatusLabel,
};

export function RunsView() {
  const [status, setStatus] = useState(anyStatus);
  const [policyId, setPolicyId] = useState(anyPolicy);
  const [query, setQuery] = useState("");
  const policies = usePolicyChoices();
  const list = useRunPages({
    status: status === anyStatus ? undefined : (status as RunStatus),
    policyId: policyId === anyPolicy ? undefined : policyId,
  });
  const runs = list.data?.pages.flatMap((page) => page.items) ?? [];
  const policyNames = new Map(
    (policies.data?.items ?? []).map((policy) => [policy.id, policy.name]),
  );
  const policyLabels: Record<string, string> = {
    [anyPolicy]: "全部策略",
    ...Object.fromEntries(policyNames),
  };
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return runs;
    return runs.filter((run) =>
      [
        policyNames.get(run.policy_id) ?? "",
        runKindLabel[run.kind],
        runStatusLabel[run.status],
      ]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [policyNames, query, runs]);
  const noRunsAtAll =
    list.isSuccess &&
    runs.length === 0 &&
    status === anyStatus &&
    policyId === anyPolicy;

  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        title="运行记录"
        description="跟踪备份进度，查看运行结果与日志。"
      />

      {!noRunsAtAll ? (
        <div className="flex flex-wrap items-end gap-3">
          <Field className="w-full sm:w-44">
            <FieldLabel htmlFor="runs-status">状态</FieldLabel>
            <Select
              items={statusLabels}
              value={status}
              onValueChange={(value) => setStatus(value as string)}
            >
              <SelectTrigger id="runs-status" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  <SelectItem value={anyStatus}>
                    {statusLabels[anyStatus]}
                  </SelectItem>
                  {runStatuses.map((item) => (
                    <SelectItem key={item} value={item}>
                      {runStatusLabel[item]}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          </Field>
          <Field className="w-full sm:w-64">
            <FieldLabel htmlFor="runs-policy">策略</FieldLabel>
            <Select
              items={policyLabels}
              value={policyId}
              onValueChange={(value) => setPolicyId(value as string)}
            >
              <SelectTrigger id="runs-policy" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  <SelectItem value={anyPolicy}>
                    {policyLabels[anyPolicy]}
                  </SelectItem>
                  {(policies.data?.items ?? []).map((policy) => (
                    <SelectItem key={policy.id} value={policy.id}>
                      {policy.name}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          </Field>
        </div>
      ) : null}
      {policies.isError ? (
        <ResourceError title="无法读取策略" error={policies.error} />
      ) : null}

      {list.isPending ? <ListSkeleton /> : null}
      {list.isError ? (
        <ResourceError error={list.error} onRetry={() => list.refetch()} />
      ) : null}

      {list.isSuccess && runs.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <ListChecksIcon />
            </EmptyMedia>
            <EmptyTitle>
              {noRunsAtAll ? "还没有运行记录" : "没有符合筛选条件的运行"}
            </EmptyTitle>
            <EmptyDescription>
              {noRunsAtAll
                ? "创建并启用保护策略后会自动运行。"
                : "调整状态或策略筛选，查看其他备份任务。"}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {runs.length > 0 ? (
        <DataPanel
          title="运行列表"
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
                <TableHead>策略</TableHead>
                <TableHead>类型</TableHead>
                <TableHead>触发</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>创建时间</TableHead>
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
              {visible.map((run) => (
                <TableRow key={run.id}>
                  <TableCell>
                    {policyNames.get(run.policy_id) ?? run.policy_id}
                  </TableCell>
                  <TableCell>{runKindLabel[run.kind]}</TableCell>
                  <TableCell>{runTriggerLabel[run.trigger]}</TableCell>
                  <TableCell>
                    <StatusBadge tone={runStatusTone(run.status)}>
                      {runStatusLabel[run.status]}
                    </StatusBadge>
                  </TableCell>
                  <TableCell>{formatDateTime(run.created_at)}</TableCell>
                  <TableCell>
                    <Link
                      className="font-medium underline-offset-4 hover:underline"
                      href={`/runs/detail?id=${run.id}`}
                    >
                      查看日志
                    </Link>
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
