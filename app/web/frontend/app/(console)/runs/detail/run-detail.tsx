"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { BanIcon } from "lucide-react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useRef, useState } from "react";
import { Diagnostics } from "@/components/resources/diagnostics";
import {
  DetailSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import {
  runStatusTone,
  StatusBadge,
} from "@/components/resources/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { useRunEvents } from "@/hooks/use-run-events";
import { useSession } from "@/hooks/use-session";
import { ApiError } from "@/lib/api/errors";
import { cancelRunMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import type { RunLogEvent } from "@/lib/api/generated/types.gen";
import { loginPath, safeNextPath } from "@/lib/navigation";
import { forgetPrincipal } from "@/lib/principal";
import { formatBytes, formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  invalidateRun,
  usePolicyChoices,
  useRun,
  useRunLog,
  useSourceChoices,
} from "@/lib/resources/queries";
import {
  isTerminalRun,
  runEventText,
  runFailureReason,
  runKindLabel,
  runLevelLabel,
  runStatusLabel,
  runTriggerLabel,
} from "@/lib/resources/runs";

const liveStatuses: Record<string, true> = { queued: true, running: true };

export function RunDetail() {
  const id = useSearchParams().get("id");
  const router = useRouter();
  const queryClient = useQueryClient();
  const session = useSession();
  const run = useRun(id);
  const log = useRunLog(id);
  const policies = usePolicyChoices();
  const sources = useSourceChoices();
  const cancel = useMutation(cancelRunMutation());
  const [liveRows, setLiveRows] = useState<RunLogEvent[]>([]);
  const [streamEnded, setStreamEnded] = useState(false);
  const seenSeq = useRef(0);

  const canCancel = hasPermission(session.data?.permissions, "runs:cancel");
  const status = run.data?.status;
  const streaming =
    id !== null &&
    log.isSuccess &&
    status !== undefined &&
    liveStatuses[status] === true &&
    !streamEnded;

  useRunEvents(id ?? "", streaming, () => seenSeq.current, {
    onLog: (event) => {
      if (event.seq > seenSeq.current) {
        seenSeq.current = event.seq;
        setLiveRows((rows) =>
          rows.some((row) => row.seq === event.seq) ? rows : [...rows, event],
        );
      }
    },
    onEnd: () => {
      setStreamEnded(true);
      if (id !== null) {
        invalidateRun(queryClient, id);
      }
    },
    // The session stopped being valid between heartbeats. This is the same
    // handover a 401 gets from the query cache: drop the cached principal
    // and return to the login page with the expiry reason.
    onRevoked: () => {
      const next = safeNextPath(
        window.location.pathname + window.location.search,
      );
      forgetPrincipal(queryClient);
      router.replace(loginPath(next, "expired"));
    },
  });

  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少运行 ID</EmptyTitle>
          <EmptyDescription>
            请从运行记录打开详情；地址形如 /runs/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (run.isPending) {
    return <DetailSkeleton />;
  }
  if (run.isError) {
    return (
      <ResourceError
        title="无法读取运行"
        error={run.error}
        onRetry={() => run.refetch()}
      />
    );
  }

  const data = run.data;
  const policyName =
    policies.data?.items.find((item) => item.id === data.policy_id)?.name ??
    data.policy_id;
  const sourceName =
    sources.data?.items.find((item) => item.id === data.source_id)?.name ??
    data.source_id;
  const logged = log.data?.items ?? [];
  const lastLoggedSeq = logged.at(-1)?.seq ?? 0;
  if (lastLoggedSeq > seenSeq.current) {
    seenSeq.current = lastLoggedSeq;
  }
  const rows = [
    ...logged,
    ...liveRows.filter((row) => row.seq > lastLoggedSeq),
  ];

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={`${runKindLabel[data.kind]}备份 · ${formatDateTime(data.started_at ?? data.created_at)}`}
        description="查看本次备份的状态、数据量与日志。"
      >
        {canCancel && !isTerminalRun(data.status) ? (
          <Button
            variant="destructive"
            disabled={cancel.isPending || data.cancel_requested}
            onClick={() =>
              cancel.mutate(
                { path: { id: data.id } },
                { onSuccess: () => invalidateRun(queryClient, data.id) },
              )
            }
          >
            {cancel.isPending ? (
              <Spinner data-icon="inline-start" />
            ) : (
              <BanIcon data-icon="inline-start" />
            )}
            {data.cancel_requested ? "已请求取消" : "取消运行"}
          </Button>
        ) : null}
      </PageHeader>

      {data.cancel_requested ? (
        <Alert>
          <BanIcon />
          <AlertTitle>已请求取消</AlertTitle>
          <AlertDescription>
            取消请求已记录。排队中的运行不会启动；正在运行的在安全点停止，已完成的备份不会被删除。
          </AlertDescription>
        </Alert>
      ) : null}
      {cancel.isError ? (
        cancel.error instanceof ApiError && cancel.error.status === 409 ? (
          <Alert>
            <BanIcon />
            <AlertTitle>运行已经结束</AlertTitle>
            <AlertDescription>运行已经结束，无法再取消。</AlertDescription>
          </Alert>
        ) : (
          <ResourceError title="取消失败" error={cancel.error} />
        )
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2">
            <StatusBadge tone={runStatusTone(data.status)}>
              {runStatusLabel[data.status]}
            </StatusBadge>
            {streaming ? <Badge variant="outline">实时更新中</Badge> : null}
          </CardTitle>
          <CardDescription>
            {runKindLabel[data.kind]}备份 · {runTriggerLabel[data.trigger]}触发
          </CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">策略</dt>
            <dd>
              <Link
                className="underline-offset-4 hover:underline"
                href={`/policies/detail?id=${data.policy_id}`}
              >
                {policyName}
              </Link>
            </dd>
            <dt className="text-muted-foreground">数据源</dt>
            <dd>
              <Link
                className="underline-offset-4 hover:underline"
                href={`/sources/detail?id=${data.source_id}`}
              >
                {sourceName}
              </Link>
            </dd>
            <dt className="text-muted-foreground">开始时间</dt>
            <dd>{formatDateTime(data.started_at)}</dd>
            <dt className="text-muted-foreground">结束时间</dt>
            <dd>{formatDateTime(data.finished_at)}</dd>
            <dt className="text-muted-foreground">耗时</dt>
            <dd>{durationText(data.started_at, data.finished_at)}</dd>
            <dt className="text-muted-foreground">数据量</dt>
            <dd>{formatBytes(data.payload_size)}</dd>
          </dl>
        </CardContent>
      </Card>

      {data.status === "failed" ? (
        <Alert variant="destructive">
          <BanIcon />
          <AlertTitle>备份失败</AlertTitle>
          <AlertDescription>
            {runFailureReason(data.error_class, data.error_code)}
          </AlertDescription>
        </Alert>
      ) : null}

      <Diagnostics title="技术诊断" description="排查问题或联系支持时展开。">
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
          <dt className="text-muted-foreground">运行标识</dt>
          <dd className="font-mono text-xs break-all">{data.id}</dd>
          <dt className="text-muted-foreground">备份标识</dt>
          <dd className="font-mono text-xs break-all">
            {data.backup_id ?? "—"}
          </dd>
          <dt className="text-muted-foreground">备份链</dt>
          <dd className="font-mono text-xs break-all">
            {data.chain_id ?? "—"}
          </dd>
          <dt className="text-muted-foreground">错误类</dt>
          <dd className="font-mono text-xs">{data.error_class ?? "—"}</dd>
          <dt className="text-muted-foreground">错误码</dt>
          <dd className="font-mono text-xs">{data.error_code ?? "—"}</dd>
          <dt className="text-muted-foreground">取消请求</dt>
          <dd>{data.cancel_requested ? "已请求" : "未请求"}</dd>
          <dt className="text-muted-foreground">计划时间</dt>
          <dd>
            {data.trigger === "manual"
              ? "—"
              : formatDateTime(data.scheduled_at)}
          </dd>
        </dl>
      </Diagnostics>

      <Card>
        <CardHeader>
          <CardTitle>日志</CardTitle>
          <CardDescription>按时间顺序记录本次备份的进度。</CardDescription>
        </CardHeader>
        <CardContent>
          {log.isPending ? <DetailSkeleton /> : null}
          {log.isError ? (
            <ResourceError
              title="无法读取日志"
              error={log.error}
              onRetry={() => log.refetch()}
            />
          ) : null}
          {log.isSuccess && rows.length === 0 ? (
            <p className="text-sm text-muted-foreground">还没有日志。</p>
          ) : null}
          {rows.length > 0 ? (
            <ol className="flex max-h-96 flex-col gap-2 overflow-y-auto text-xs">
              {rows.map((row) => (
                <li key={row.seq} className="flex flex-wrap items-center gap-2">
                  <span className="w-8 shrink-0 text-right text-muted-foreground">
                    {row.seq}
                  </span>
                  <StatusBadge
                    tone={
                      row.level === "error" || row.level === "warn"
                        ? "danger"
                        : "neutral"
                    }
                  >
                    {runLevelLabel[row.level] ?? row.level}
                  </StatusBadge>
                  <span className="font-medium" title={row.event}>
                    {runEventText(row.event)}
                  </span>
                  <span className="min-w-0 flex-1 whitespace-pre-wrap break-words text-muted-foreground">
                    {row.message}
                  </span>
                </li>
              ))}
            </ol>
          ) : null}
          {log.data?.truncated ? (
            <p className="mt-2 text-sm text-muted-foreground">
              日志已达到上限，更早的记录不再保留。
            </p>
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}

function durationText(start: string | null, end: string | null): string {
  if (start === null || end === null) return "—";
  const seconds = Math.max(
    0,
    Math.round((new Date(end).getTime() - new Date(start).getTime()) / 1000),
  );
  if (Number.isNaN(seconds)) return "—";
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes} 分 ${seconds % 60} 秒`;
}
