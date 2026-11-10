"use client";

import { HistoryIcon, PlusIcon } from "lucide-react";
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
import {
  restoreStatusTone,
  StatusBadge,
} from "@/components/resources/status-badge";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useSession } from "@/hooks/use-session";
import type { RestoreStatus } from "@/lib/api/generated/types.gen";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import { useRestorePages } from "@/lib/resources/queries";
import {
  restorePhaseLabel,
  restoreStatuses,
  restoreStatusLabel,
  restoreVerifyStatusLabel,
} from "@/lib/resources/restore";

const anyStatus = "any";

const statusLabels: Record<string, string> = {
  [anyStatus]: "全部状态",
  ...restoreStatusLabel,
};

export function RestoresView() {
  const session = useSession();
  const canPlan = hasPermission(session.data?.permissions, "restores:plan");
  const [status, setStatus] = useState(anyStatus);
  const [query, setQuery] = useState("");
  const list = useRestorePages({
    status: status === anyStatus ? undefined : (status as RestoreStatus),
  });
  const restores = list.data?.pages.flatMap((page) => page.items) ?? [];
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return restores;
    return restores.filter((restore) =>
      [
        restoreStatusLabel[restore.status],
        restore.phase ? restorePhaseLabel[restore.phase] : "",
        restoreVerifyStatusLabel[restore.verify_status],
      ]
        .join(" ")
        .toLowerCase()
        .includes(needle),
    );
  }, [query, restores]);

  return (
    <div className="flex flex-col gap-5">
      <PageHeader title="恢复" description="查看隔离恢复与演练结果。">
        {canPlan ? (
          <Button nativeButton={false} render={<Link href="/restores/new" />}>
            <PlusIcon data-icon="inline-start" />
            新建恢复
          </Button>
        ) : null}
      </PageHeader>

      <Field className="w-full sm:w-44">
        <FieldLabel htmlFor="restores-status">状态</FieldLabel>
        <Select
          items={statusLabels}
          value={status}
          onValueChange={(value) => setStatus(value as string)}
        >
          <SelectTrigger id="restores-status" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              <SelectItem value={anyStatus}>
                {statusLabels[anyStatus]}
              </SelectItem>
              {restoreStatuses.map((item) => (
                <SelectItem key={item} value={item}>
                  {restoreStatusLabel[item]}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
      </Field>

      {list.isPending ? <ListSkeleton /> : null}
      {list.isError ? (
        <ResourceError error={list.error} onRetry={() => list.refetch()} />
      ) : null}

      {list.isSuccess && restores.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <HistoryIcon />
            </EmptyMedia>
            <EmptyTitle>没有符合条件的恢复</EmptyTitle>
            <EmptyDescription>
              恢复从一份已完成的备份点开始；当前筛选下没有记录。
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {restores.length > 0 ? (
        <DataPanel
          title="恢复记录"
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
                <TableHead>状态</TableHead>
                <TableHead>进度</TableHead>
                <TableHead>演练</TableHead>
                <TableHead>创建时间</TableHead>
                <TableHead>操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="h-20 text-center">
                    没有匹配的记录
                  </TableCell>
                </TableRow>
              ) : null}
              {visible.map((restore) => (
                <TableRow key={restore.id}>
                  <TableCell>
                    <StatusBadge tone={restoreStatusTone(restore.status)}>
                      {restoreStatusLabel[restore.status]}
                    </StatusBadge>
                  </TableCell>
                  <TableCell>
                    {restore.phase ? restorePhaseLabel[restore.phase] : "—"}
                  </TableCell>
                  <TableCell>
                    {restoreVerifyStatusLabel[restore.verify_status]}
                  </TableCell>
                  <TableCell>{formatDateTime(restore.created_at)}</TableCell>
                  <TableCell>
                    <RowActions
                      label="恢复"
                      actions={[
                        {
                          label: "查看",
                          href: `/restores/detail?id=${restore.id}`,
                        },
                        {
                          label: "备份点",
                          href: `/backups/detail?id=${restore.backup_id}`,
                        },
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
