"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { HistoryIcon, RotateCwIcon } from "lucide-react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { ReplicaStatusBadge } from "@/components/resources/replica-status";
import {
  DetailSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
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
import { toast } from "@/components/ui/toast";
import { useSession } from "@/hooks/use-session";
import { retryReplicaMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import { formatBytes, formatDateTime, formatLsn } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  invalidateBackup,
  invalidateReplicas,
  useBackup,
  useRepositoryChoices,
} from "@/lib/resources/queries";
import { backupKindText, backupStatusText } from "@/lib/resources/runs";

export function BackupDetail() {
  const id = useSearchParams().get("id");
  const queryClient = useQueryClient();
  const session = useSession();
  const backup = useBackup(id);
  const repositories = useRepositoryChoices();
  const retry = useMutation(retryReplicaMutation());
  const canRetry = hasPermission(session.data?.permissions, "replicas:retry");

  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少备份 ID</EmptyTitle>
          <EmptyDescription>
            请从运行详情或数据源的备份链打开；地址形如 /backups/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (backup.isPending) {
    return <DetailSkeleton />;
  }
  if (backup.isError) {
    return (
      <ResourceError
        title="无法读取备份"
        error={backup.error}
        onRetry={() => backup.refetch()}
      />
    );
  }

  const data = backup.data;
  const repositoryNames = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );
  const retrying = retry.isPending ? retry.variables : undefined;

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={`${backupKindText(data.kind)}备份`}
        description="LSN、大小、校验和、本地是否仍在，以及每个仓库的副本状态。"
      >
        {hasPermission(session.data?.permissions, "restores:plan") &&
        data.status === "completed" ? (
          <Button
            variant="outline"
            nativeButton={false}
            render={<Link href={`/restores/new?backup_id=${data.id}`} />}
          >
            <HistoryIcon data-icon="inline-start" />
            恢复到此备份
          </Button>
        ) : null}
      </PageHeader>

      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm">{data.id}</span>
            <Badge
              variant={data.status === "failed" ? "destructive" : "secondary"}
            >
              {backupStatusText(data.status)}
            </Badge>
            <Badge variant={data.local_present ? "secondary" : "outline"}>
              {data.local_present ? "本地存在" : "本地已不在"}
            </Badge>
          </CardTitle>
          <CardDescription>
            链 {data.chain_id} · 序号 {data.seq}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">起始 LSN</dt>
            <dd className="font-mono text-xs">{formatLsn(data.from_lsn)}</dd>
            <dt className="text-muted-foreground">结束 LSN</dt>
            <dd className="font-mono text-xs">{formatLsn(data.to_lsn)}</dd>
            <dt className="text-muted-foreground">最后 LSN</dt>
            <dd className="font-mono text-xs">{formatLsn(data.last_lsn)}</dd>
            <dt className="text-muted-foreground">大小</dt>
            <dd>{formatBytes(data.payload_size)}</dd>
            <dt className="text-muted-foreground">SHA-256</dt>
            <dd className="font-mono text-xs break-all">
              {data.payload_sha256 === "" ? "—" : data.payload_sha256}
            </dd>
            <dt className="text-muted-foreground">对象路径</dt>
            <dd className="font-mono text-xs break-all">
              {data.object_path === "" ? "—" : data.object_path}
            </dd>
            <dt className="text-muted-foreground">完成时间</dt>
            <dd>{formatDateTime(data.completed_at)}</dd>
            <dt className="text-muted-foreground">父备份</dt>
            <dd className="font-mono text-xs">{data.parent_id ?? "—"}</dd>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>各仓库副本</CardTitle>
          <CardDescription>
            每个副本仓库一行；已上传不等于已校验，只有已校验的副本推进异地保护点。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {data.replicas.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              这份备份没有副本记录。
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
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
                {data.replicas.map((replica) => {
                  const pending =
                    retrying?.path.id === data.id &&
                    retrying.path.repository_id === replica.repository_id;
                  return (
                    <TableRow key={replica.repository_id}>
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
                      <TableCell>
                        {formatDateTime(replica.alert_since)}
                      </TableCell>
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
                                    id: data.id,
                                    repository_id: replica.repository_id,
                                  },
                                },
                                {
                                  onSuccess: () => {
                                    invalidateBackup(queryClient, data.id);
                                    invalidateReplicas(queryClient);
                                    toast.add({
                                      type: "success",
                                      title: "已安排重试",
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
          )}
          {retry.isError ? (
            <div className="mt-3">
              <ResourceError title="重试失败" error={retry.error} />
            </div>
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}
