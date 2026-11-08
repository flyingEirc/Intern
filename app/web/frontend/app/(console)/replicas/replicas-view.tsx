"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CopyIcon, RotateCwIcon } from "lucide-react";
import Link from "next/link";
import { useState } from "react";
import { ReplicaStatusBadge } from "@/components/resources/replica-status";
import {
  ListSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
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
import { toast } from "@/components/ui/toast";
import { useSession } from "@/hooks/use-session";
import { retryReplicaMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import type { ReplicaStatus } from "@/lib/api/generated/types.gen";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  invalidateBackup,
  invalidateReplicas,
  useReplicaQueue,
  useRepositoryChoices,
} from "@/lib/resources/queries";
import { replicaStatuses, replicaStatusLabel } from "@/lib/resources/runs";

const anyStatus = "any";
const anyRepository = "any";

const statusLabels: Record<string, string> = {
  [anyStatus]: "全部状态",
  ...replicaStatusLabel,
};

export function ReplicasView() {
  const session = useSession();
  const queryClient = useQueryClient();
  const [status, setStatus] = useState(anyStatus);
  const [repositoryId, setRepositoryId] = useState(anyRepository);
  const repositories = useRepositoryChoices();
  const queue = useReplicaQueue({
    status: status === anyStatus ? undefined : (status as ReplicaStatus),
    repositoryId: repositoryId === anyRepository ? undefined : repositoryId,
  });
  const retry = useMutation(retryReplicaMutation());
  const canRetry = hasPermission(session.data?.permissions, "replicas:retry");
  const replicas = queue.data?.items ?? [];
  const repositoryNames = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );
  const repositoryLabels: Record<string, string> = {
    [anyRepository]: "全部仓库",
    ...Object.fromEntries(repositoryNames),
  };
  const retrying = retry.isPending ? retry.variables : undefined;

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="副本复制"
        description="各仓库复制队列的前 200 条，按仓库内的排队顺序。失败与耗尽的副本可以由具备重试权限的账号立即重试。"
      />

      <div className="flex flex-wrap items-end gap-3">
        <Field className="w-44">
          <FieldLabel htmlFor="replicas-status">状态</FieldLabel>
          <Select
            items={statusLabels}
            value={status}
            onValueChange={(value) => setStatus(value as string)}
          >
            <SelectTrigger id="replicas-status" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={anyStatus}>
                  {statusLabels[anyStatus]}
                </SelectItem>
                {replicaStatuses.map((item) => (
                  <SelectItem key={item} value={item}>
                    {replicaStatusLabel[item]}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
        <Field className="w-64">
          <FieldLabel htmlFor="replicas-repository">仓库</FieldLabel>
          <Select
            items={repositoryLabels}
            value={repositoryId}
            onValueChange={(value) => setRepositoryId(value as string)}
          >
            <SelectTrigger id="replicas-repository" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value={anyRepository}>
                  {repositoryLabels[anyRepository]}
                </SelectItem>
                {(repositories.data?.items ?? []).map((repository) => (
                  <SelectItem key={repository.id} value={repository.id}>
                    {repository.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
      </div>

      {queue.isPending ? <ListSkeleton /> : null}
      {queue.isError ? (
        <ResourceError error={queue.error} onRetry={() => queue.refetch()} />
      ) : null}

      {queue.isSuccess && replicas.length === 0 ? (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <CopyIcon />
            </EmptyMedia>
            <EmptyTitle>复制队列为空</EmptyTitle>
            <EmptyDescription>
              备份完成后，需要异地副本的备份会进入这里排队。
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {replicas.length > 0 ? (
        <div className="flex flex-col gap-3">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>备份</TableHead>
                <TableHead>仓库</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>尝试</TableHead>
                <TableHead>下次尝试</TableHead>
                <TableHead>告警起点</TableHead>
                <TableHead>错误码</TableHead>
                <TableHead>FTP 应答</TableHead>
                {canRetry ? <TableHead>操作</TableHead> : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {replicas.map((replica) => {
                const pending =
                  retrying?.path.id === replica.backup_id &&
                  retrying.path.repository_id === replica.repository_id;
                return (
                  <TableRow
                    key={`${replica.backup_id}:${replica.repository_id}`}
                  >
                    <TableCell>
                      <Link
                        className="font-mono text-xs underline-offset-4 hover:underline"
                        href={`/backups/detail?id=${replica.backup_id}`}
                      >
                        {replica.backup_id}
                      </Link>
                    </TableCell>
                    <TableCell>
                      {repositoryNames.get(replica.repository_id) ??
                        replica.repository_id}
                    </TableCell>
                    <TableCell>
                      <ReplicaStatusBadge status={replica.status} />
                    </TableCell>
                    <TableCell>{replica.attempts}</TableCell>
                    <TableCell>
                      {formatDateTime(replica.next_attempt_at)}
                    </TableCell>
                    <TableCell>{formatDateTime(replica.alert_since)}</TableCell>
                    <TableCell className="font-mono text-xs">
                      {replica.last_error_code === ""
                        ? "—"
                        : replica.last_error_code}
                    </TableCell>
                    <TableCell>
                      {replica.last_reply_code === 0
                        ? "—"
                        : replica.last_reply_code}
                    </TableCell>
                    {canRetry ? (
                      <TableCell>
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={retry.isPending}
                          onClick={() =>
                            retry.mutate(
                              {
                                path: {
                                  id: replica.backup_id,
                                  repository_id: replica.repository_id,
                                },
                              },
                              {
                                onSuccess: () => {
                                  invalidateReplicas(queryClient);
                                  invalidateBackup(
                                    queryClient,
                                    replica.backup_id,
                                  );
                                  toast.add({
                                    type: "success",
                                    title: "已安排重试",
                                    description:
                                      "副本已回到等待复制，尝试次数已清零。",
                                  });
                                },
                              },
                            )
                          }
                        >
                          {pending ? (
                            <Spinner data-icon="inline-start" />
                          ) : (
                            <RotateCwIcon data-icon="inline-start" />
                          )}
                          重试
                        </Button>
                      </TableCell>
                    ) : null}
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
          {retry.isError ? (
            <ResourceError title="重试失败" error={retry.error} />
          ) : null}
          <p className="text-sm text-muted-foreground">
            队列接口只返回有限的一页，没有继续向后翻页的游标。
          </p>
        </div>
      ) : null}
    </div>
  );
}
