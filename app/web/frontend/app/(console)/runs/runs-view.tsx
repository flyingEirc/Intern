"use client";

import { ListChecksIcon } from "lucide-react";
import Link from "next/link";
import { useState } from "react";
import {
  ListSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import { Spinner } from "@/components/ui/spinner";
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

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="运行记录"
        description="按创建顺序最新在前。状态与策略筛选为精确匹配，由服务端执行。"
      />

      <div className="flex flex-wrap items-end gap-3">
        <Field className="w-44">
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
        <Field className="w-64">
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
            <EmptyTitle>没有符合条件的运行</EmptyTitle>
            <EmptyDescription>
              运行由策略调度或在策略详情页手动触发；当前筛选下没有记录。
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {runs.length > 0 ? (
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>运行</TableHead>
                <TableHead>策略</TableHead>
                <TableHead>类型</TableHead>
                <TableHead>触发</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>创建时间</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {runs.map((run) => (
                <TableRow key={run.id}>
                  <TableCell>
                    <Link
                      className="font-mono text-xs underline-offset-4 hover:underline"
                      href={`/runs/detail?id=${run.id}`}
                    >
                      {run.id}
                    </Link>
                  </TableCell>
                  <TableCell>
                    {policyNames.get(run.policy_id) ?? run.policy_id}
                  </TableCell>
                  <TableCell>{runKindLabel[run.kind]}</TableCell>
                  <TableCell>{runTriggerLabel[run.trigger]}</TableCell>
                  <TableCell>
                    <Badge
                      variant={
                        run.status === "failed" ? "destructive" : "outline"
                      }
                    >
                      {runStatusLabel[run.status]}
                    </Badge>
                  </TableCell>
                  <TableCell>{formatDateTime(run.created_at)}</TableCell>
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
