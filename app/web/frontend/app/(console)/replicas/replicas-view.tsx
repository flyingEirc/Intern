"use client";

import { useMutation, useQueries, useQueryClient } from "@tanstack/react-query";
import { ChevronDownIcon, CopyIcon, RotateCwIcon } from "lucide-react";
import Link from "next/link";
import { useState } from "react";
import { DataPanel } from "@/components/resources/data-panel";
import { ReplicaStatusBadge } from "@/components/resources/replica-status";
import {
  ListSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
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
import {
  getBackupOptions,
  retryReplicaMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import type { Replica, ReplicaStatus } from "@/lib/api/generated/types.gen";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  invalidateBackup,
  invalidateReplicas,
  useReplicaQueue,
  useRepositoryChoices,
} from "@/lib/resources/queries";
import {
  backupKindText,
  replicaStatuses,
  replicaStatusLabel,
} from "@/lib/resources/runs";

const anyStatus = "any";
const anyRepository = "any";

const statusLabels: Record<string, string> = {
  [anyStatus]: "全部状态",
  ...replicaStatusLabel,
};

/**
 * The retry endpoint resets any row to pending with a fresh attempt budget,
 * but only a failed copy needs one: queued, copying, uploaded and verified
 * rows are already on their way to protection.
 */
function canRetryReplica(status: ReplicaStatus): boolean {
  return status === "failed" || status === "exhausted";
}

function replicaKey(replica: Replica): string {
  return `${replica.backup_id}:${replica.repository_id}`;
}

export function ReplicasView() {
  const session = useSession();
  const queryClient = useQueryClient();
  const [status, setStatus] = useState(anyStatus);
  const [repositoryId, setRepositoryId] = useState(anyRepository);
  const [query, setQuery] = useState("");
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
  const backupIds = [...new Set(replicas.map((replica) => replica.backup_id))];
  const backups = useQueries({
    queries: backupIds.map((id) => getBackupOptions({ path: { id } })),
  });
  const backupById = new Map(
    backupIds.map((id, index) => [id, backups[index]?.data]),
  );
  const retrying = retry.isPending ? retry.variables : undefined;
  const visible = replicas.filter((replica) => {
    const needle = query.trim().toLowerCase();
    if (needle === "") return true;
    const backup = backupById.get(replica.backup_id);
    return [
      backup === undefined ? "" : backupKindText(backup.kind),
      backup?.completed_at === undefined
        ? ""
        : formatDateTime(backup.completed_at),
      repositoryNames.get(replica.repository_id) ?? "",
      replicaStatusLabel[replica.status],
      formatDateTime(replica.verified_at),
    ]
      .join(" ")
      .toLowerCase()
      .includes(needle);
  });

  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        title="异地副本"
        description="查看异地备份的复制与校验结果，及时处理失败任务。"
      />

      <div className="flex flex-wrap items-end gap-3">
        <Field className="w-full sm:w-44">
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
        <Field className="w-full sm:w-64">
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
        <DataPanel
          title="复制队列"
          description="查看复制进度与需要处理的任务。"
          count={visible.length}
          search={query}
          onSearch={setQuery}
        >
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>备份点</TableHead>
                <TableHead>仓库</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>校验时间</TableHead>
                <TableHead>失败详情</TableHead>
                {canRetry ? <TableHead>操作</TableHead> : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {visible.length === 0 ? (
                <TableRow>
                  <TableCell
                    colSpan={canRetry ? 6 : 5}
                    className="h-20 text-center"
                  >
                    没有匹配的记录
                  </TableCell>
                </TableRow>
              ) : null}
              {visible.map((replica) => {
                const backup = backupById.get(replica.backup_id);
                const pending =
                  retrying?.path.id === replica.backup_id &&
                  retrying.path.repository_id === replica.repository_id;
                const failed =
                  replica.status === "failed" || replica.status === "exhausted";
                const point =
                  backup === undefined
                    ? null
                    : `${backupKindText(backup.kind)} · ${formatDateTime(backup.completed_at)}`;
                return (
                  <TableRow key={replicaKey(replica)}>
                    <TableCell>
                      <Link
                        className="font-medium text-primary underline-offset-4 hover:underline focus-visible:rounded-sm focus-visible:outline-2 focus-visible:outline-ring"
                        href={`/backups/detail?id=${replica.backup_id}`}
                      >
                        {point ?? "查看备份点"}
                      </Link>
                    </TableCell>
                    <TableCell>
                      {repositoryNames.get(replica.repository_id) ??
                        replica.repository_id}
                    </TableCell>
                    <TableCell>
                      <ReplicaStatusBadge status={replica.status} />
                    </TableCell>
                    <TableCell>{formatDateTime(replica.verified_at)}</TableCell>
                    <TableCell>
                      {failed ? (
                        <Collapsible>
                          <CollapsibleTrigger
                            render={<Button variant="ghost" size="sm" />}
                          >
                            查看失败详情
                            <ChevronDownIcon data-icon="inline-end" />
                          </CollapsibleTrigger>
                          <CollapsibleContent>
                            <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
                              <dt className="text-muted-foreground">
                                尝试次数
                              </dt>
                              <dd>{replica.attempts}</dd>
                              <dt className="text-muted-foreground">
                                下次尝试
                              </dt>
                              <dd>{formatDateTime(replica.next_attempt_at)}</dd>
                              <dt className="text-muted-foreground">
                                告警起点
                              </dt>
                              <dd>{formatDateTime(replica.alert_since)}</dd>
                              <dt className="text-muted-foreground">错误码</dt>
                              <dd className="font-mono">
                                {replica.last_error_code === ""
                                  ? "—"
                                  : replica.last_error_code}
                              </dd>
                              <dt className="text-muted-foreground">
                                FTP 应答
                              </dt>
                              <dd>
                                {replica.last_reply_code === 0
                                  ? "—"
                                  : replica.last_reply_code}
                              </dd>
                            </dl>
                          </CollapsibleContent>
                        </Collapsible>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                    {canRetry ? (
                      <TableCell>
                        {canRetryReplica(replica.status) ? (
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
                        ) : (
                          "—"
                        )}
                      </TableCell>
                    ) : null}
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
          {retry.isError ? (
            <div className="p-4">
              <ResourceError title="重试失败" error={retry.error} />
            </div>
          ) : null}
        </DataPanel>
      ) : null}
    </div>
  );
}
