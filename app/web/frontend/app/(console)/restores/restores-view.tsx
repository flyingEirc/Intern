"use client";

import { HistoryIcon, PlusIcon } from "lucide-react";
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
  const list = useRestorePages({
    status: status === anyStatus ? undefined : (status as RestoreStatus),
  });
  const restores = list.data?.pages.flatMap((page) => page.items) ?? [];

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="恢复"
        description="按创建顺序最新在前。状态筛选为精确匹配，由服务端执行。"
      >
        {canPlan ? (
          <Button nativeButton={false} render={<Link href="/restores/new" />}>
            <PlusIcon data-icon="inline-start" />
            新建恢复
          </Button>
        ) : null}
      </PageHeader>

      <Field className="w-44">
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
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>恢复</TableHead>
                <TableHead>备份点</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>阶段</TableHead>
                <TableHead>演练</TableHead>
                <TableHead>创建时间</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {restores.map((restore) => (
                <TableRow key={restore.id}>
                  <TableCell>
                    <Link
                      className="font-mono text-xs underline-offset-4 hover:underline"
                      href={`/restores/detail?id=${restore.id}`}
                    >
                      {restore.id}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Link
                      className="font-mono text-xs underline-offset-4 hover:underline"
                      href={`/backups/detail?id=${restore.backup_id}`}
                    >
                      {restore.backup_id}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant={
                        restore.status === "failed" ? "destructive" : "outline"
                      }
                    >
                      {restoreStatusLabel[restore.status]}
                    </Badge>
                  </TableCell>
                  <TableCell>
                    {restore.phase ? restorePhaseLabel[restore.phase] : "—"}
                  </TableCell>
                  <TableCell>
                    {restoreVerifyStatusLabel[restore.verify_status]}
                  </TableCell>
                  <TableCell>{formatDateTime(restore.created_at)}</TableCell>
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
