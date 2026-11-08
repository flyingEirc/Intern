"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { BanIcon, CopyIcon } from "lucide-react";
import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { useState } from "react";
import {
  DetailSkeleton,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
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
import { ApiError } from "@/lib/api/errors";
import { cancelRestoreMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import type { Restore, RestoreLog } from "@/lib/api/generated/types.gen";
import { formatBytes, formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  invalidateRestore,
  useRepositoryChoices,
  useRestore,
  useRestoreLog,
} from "@/lib/resources/queries";
import {
  exportStartCommand,
  isTerminalRestore,
  linkSourceLabel,
  phasesRecorded,
  restoreErrorClassLabel,
  restorePhaseLabel,
  restorePhaseOrder,
  restoreStatusLabel,
  restoreVerifyStatusLabel,
  riskText,
  shortenDigest,
  sideEffectStatements,
} from "@/lib/resources/restore";

export function RestoreDetail() {
  const id = useSearchParams().get("id");
  const session = useSession();
  const restore = useRestore(id);
  const live =
    restore.data === undefined
      ? undefined
      : !isTerminalRestore(restore.data.status);
  const log = useRestoreLog(id, live);
  const canCancel = hasPermission(
    session.data?.permissions,
    "restores:execute",
  );

  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少恢复 ID</EmptyTitle>
          <EmptyDescription>
            请从恢复记录打开详情；地址形如 /restores/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (restore.isPending) {
    return <DetailSkeleton />;
  }
  if (restore.isError) {
    return (
      <ResourceError
        title="无法读取恢复"
        error={restore.error}
        onRetry={() => restore.refetch()}
      />
    );
  }

  return (
    <RestoreBody
      data={restore.data}
      live={!isTerminalRestore(restore.data.status)}
      canCancel={canCancel}
      logs={log.data?.items ?? []}
      truncated={log.data?.truncated ?? false}
      logPending={log.isPending}
      logError={log.isError ? log.error : null}
      onRetryLog={() => log.refetch()}
    />
  );
}

function RestoreBody({
  data,
  live,
  canCancel,
  logs,
  truncated,
  logPending,
  logError,
  onRetryLog,
}: {
  data: Restore;
  live: boolean;
  canCancel: boolean;
  logs: RestoreLog[];
  truncated: boolean;
  logPending: boolean;
  logError: unknown;
  onRetryLog: () => void;
}) {
  const queryClient = useQueryClient();
  const repositories = useRepositoryChoices();
  const cancel = useMutation(cancelRestoreMutation());
  const [cancelOpen, setCancelOpen] = useState(false);
  const repositoryNames = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );
  const recorded = phasesRecorded(logs);
  const published =
    data.side_effects?.export_published === true && data.export_path !== null;

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="恢复详情"
        description="状态、阶段、日志与终态副作用。执行中每 2 秒刷新；恢复没有实时事件流。"
      >
        {canCancel && !isTerminalRestore(data.status) ? (
          <AlertDialog open={cancelOpen} onOpenChange={setCancelOpen}>
            <AlertDialogTrigger
              render={
                <Button
                  variant="destructive"
                  disabled={cancel.isPending || data.cancel_requested}
                />
              }
            >
              {cancel.isPending ? (
                <Spinner data-icon="inline-start" />
              ) : (
                <BanIcon data-icon="inline-start" />
              )}
              {data.cancel_requested ? "已请求取消" : "取消恢复"}
            </AlertDialogTrigger>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>取消这次恢复？</AlertDialogTitle>
                <AlertDialogDescription>
                  排队中的恢复不会启动。正在执行的会在安全点停止，已发布的导出不会被删除。终态恢复不能再取消。
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={cancel.isPending}>
                  返回
                </AlertDialogCancel>
                <AlertDialogAction
                  variant="destructive"
                  disabled={cancel.isPending}
                  onClick={() => {
                    cancel.mutate(
                      { path: { id: data.id } },
                      {
                        onSuccess: () => {
                          setCancelOpen(false);
                          invalidateRestore(queryClient, data.id);
                        },
                      },
                    );
                  }}
                >
                  {cancel.isPending ? (
                    <Spinner data-icon="inline-start" />
                  ) : null}
                  确认取消
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        ) : null}
      </PageHeader>

      {data.cancel_requested ? (
        <Alert>
          <BanIcon />
          <AlertTitle>已请求取消</AlertTitle>
          <AlertDescription>
            取消请求已记录。排队中的恢复不会启动；正在执行的在安全点停止。已发布的导出不会被删除。
          </AlertDescription>
        </Alert>
      ) : null}
      {cancel.isError ? (
        cancel.error instanceof ApiError && cancel.error.status === 409 ? (
          <Alert>
            <BanIcon />
            <AlertTitle>恢复已经结束</AlertTitle>
            <AlertDescription>
              终态恢复不能再取消，状态以服务端为准。
            </AlertDescription>
          </Alert>
        ) : (
          <ResourceError title="取消失败" error={cancel.error} />
        )
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm">{data.id}</span>
            <Badge
              variant={data.status === "failed" ? "destructive" : "secondary"}
            >
              {restoreStatusLabel[data.status]}
            </Badge>
            {live && data.phase ? (
              <Badge variant="outline">{restorePhaseLabel[data.phase]}</Badge>
            ) : null}
            {live ? <Badge variant="outline">每 2 秒刷新</Badge> : null}
          </CardTitle>
          <CardDescription>
            备份点{" "}
            <Link
              className="font-mono text-xs underline-offset-4 hover:underline"
              href={`/backups/detail?id=${data.backup_id}`}
            >
              {data.backup_id}
            </Link>
          </CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">创建</dt>
            <dd>{formatDateTime(data.created_at)}</dd>
            <dt className="text-muted-foreground">开始</dt>
            <dd>{formatDateTime(data.started_at)}</dd>
            <dt className="text-muted-foreground">结束</dt>
            <dd>{formatDateTime(data.finished_at)}</dd>
            <dt className="text-muted-foreground">计划摘要</dt>
            <dd className="font-mono text-xs" title={data.plan_digest}>
              {shortenDigest(data.plan_digest)}
            </dd>
            <dt className="text-muted-foreground">导出路径</dt>
            <dd className="font-mono text-xs break-all">
              {data.export_path ?? "—"}
            </dd>
            <dt className="text-muted-foreground">导出大小</dt>
            <dd>{formatBytes(data.export_bytes)}</dd>
            <dt className="text-muted-foreground">错误类</dt>
            <dd>
              {data.error_class
                ? restoreErrorClassLabel[data.error_class]
                : "—"}
            </dd>
            <dt className="text-muted-foreground">错误码</dt>
            <dd className="font-mono text-xs">{data.error_code ?? "—"}</dd>
          </dl>
        </CardContent>
      </Card>

      {isTerminalRestore(data.status) ? (
        <Card>
          <CardHeader>
            <CardTitle>终态副作用</CardTitle>
            <CardDescription>
              这三项记录这次恢复实际留下了什么，不是对已发生写入的撤销。
            </CardDescription>
          </CardHeader>
          <CardContent>
            {data.side_effects ? (
              <ul className="flex flex-col gap-1 text-sm">
                {sideEffectStatements(data.side_effects).map((statement) => (
                  <li key={statement}>{statement}</li>
                ))}
              </ul>
            ) : (
              <p className="text-sm text-muted-foreground">
                服务端没有返回副作用记录。
              </p>
            )}
          </CardContent>
        </Card>
      ) : null}

      {published ? (
        <Card>
          <CardHeader>
            <CardTitle>用新的 MySQL 容器启动导出</CardTitle>
            <CardDescription>
              导出已发布到 {data.export_path}
              。下面的命令与运维文档一致：宿主机目录映射到容器的
              /exports，镜像取部署锁定的 MySQL 镜像。Intern 从不自动替换生产
              datadir。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <pre className="overflow-x-auto rounded-lg bg-muted p-3 font-mono text-xs whitespace-pre">
              {exportStartCommand(data.id)}
            </pre>
            <div>
              <Button
                variant="outline"
                size="sm"
                onClick={() => {
                  void navigator.clipboard
                    .writeText(exportStartCommand(data.id))
                    .then(
                      () =>
                        toast.add({ type: "success", title: "已复制启动命令" }),
                      () =>
                        toast.add({ type: "error", title: "无法复制启动命令" }),
                    );
                }}
              >
                <CopyIcon data-icon="inline-start" />
                复制命令
              </Button>
            </div>
          </CardContent>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2">
            演练
            <Badge
              variant={
                data.verify_status === "failed"
                  ? "destructive"
                  : data.verify_status === "passed"
                    ? "secondary"
                    : "outline"
              }
            >
              {restoreVerifyStatusLabel[data.verify_status]}
            </Badge>
          </CardTitle>
          <CardDescription>
            {data.rehearse ? "这次恢复请求了演练。" : "这次恢复没有请求演练。"}
            {data.verify_sql.length > 0
              ? ` 确认的校验语句：${data.verify_sql.length} 条。`
              : ""}
          </CardDescription>
        </CardHeader>
        <CardContent>
          <VerifyResult result={data.verify_result} />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>计划</CardTitle>
          <CardDescription>
            确认时的计划：目标 LSN {data.plan.target_to_lsn}，导出路径{" "}
            <span className="font-mono text-xs">{data.plan.export_path}</span>
            。工作空间需要 {formatBytes(data.plan.required_work_bytes)}
            （当时可用 {formatBytes(data.plan.available_work_bytes)}），导出需要{" "}
            {formatBytes(data.plan.required_export_bytes)}（当时可用{" "}
            {formatBytes(data.plan.available_export_bytes)}）。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>序号</TableHead>
                <TableHead>来源</TableHead>
                <TableHead>仓库</TableHead>
                <TableHead>LSN</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.plan.links.map((link) => (
                <TableRow key={link.backup_id}>
                  <TableCell>{link.seq}</TableCell>
                  <TableCell>{linkSourceLabel[link.source]}</TableCell>
                  <TableCell>
                    {link.repository_id === ""
                      ? "—"
                      : (repositoryNames.get(link.repository_id) ??
                        link.repository_id)}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {link.from_lsn} → {link.to_lsn}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <div className="flex flex-col gap-1 text-sm">
            {data.plan.risks.map((code) => (
              <p key={code}>
                <span className="font-mono text-xs">{code}</span>：
                {riskText(code)}
              </p>
            ))}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>日志与阶段</CardTitle>
          <CardDescription>
            阶段顺序以日志为准：
            {restorePhaseOrder
              .map((phase) => restorePhaseLabel[phase])
              .join(" → ")}
            。已记录：
            {recorded.length === 0
              ? "还没有阶段"
              : recorded.map((phase) => restorePhaseLabel[phase]).join(" → ")}
            。每个恢复最多保留 1000 条。
          </CardDescription>
        </CardHeader>
        <CardContent>
          {logPending ? <DetailSkeleton /> : null}
          {logError ? (
            <ResourceError
              title="无法读取日志"
              error={logError}
              onRetry={onRetryLog}
            />
          ) : null}
          {!logPending && logs.length === 0 ? (
            <p className="text-sm text-muted-foreground">还没有日志。</p>
          ) : null}
          {logs.length > 0 ? (
            <ol className="flex max-h-96 flex-col gap-1 overflow-y-auto font-mono text-xs">
              {logs.map((row) => (
                <li key={row.seq} className="flex gap-3">
                  <span className="w-10 shrink-0 text-right text-muted-foreground">
                    {row.seq}
                  </span>
                  <span className="w-14 shrink-0 text-muted-foreground">
                    {row.level}
                  </span>
                  <span className="whitespace-pre-wrap">{row.message}</span>
                </li>
              ))}
            </ol>
          ) : null}
          {truncated ? (
            <p className="mt-2 text-sm text-muted-foreground">
              日志已达到上限，更早的记录不再保留。
            </p>
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}

function VerifyResult({ result }: { result: Restore["verify_result"] }) {
  if (result === null) {
    return <p className="text-sm text-muted-foreground">没有演练结果。</p>;
  }
  const queries = Array.isArray(result.queries) ? result.queries : [];
  return (
    <div className="flex flex-col gap-3">
      {queries.length === 0 ? (
        <p className="text-sm text-muted-foreground">演练没有逐条语句结果。</p>
      ) : (
        queries.map((item) => (
          <QueryResult
            key={
              typeof item === "object" && item !== null && "sql" in item
                ? String(item.sql)
                : JSON.stringify(item)
            }
            item={item}
          />
        ))
      )}
    </div>
  );
}

function QueryResult({ item }: { item: unknown }) {
  if (typeof item !== "object" || item === null) {
    return null;
  }
  const record = item as { [key: string]: unknown };
  const sql = typeof record.sql === "string" ? record.sql : "";
  const errorCode =
    typeof record.error_code === "string" ? record.error_code : "";
  const columns = Array.isArray(record.columns)
    ? record.columns.filter(
        (column): column is string => typeof column === "string",
      )
    : [];
  const rows = Array.isArray(record.rows) ? record.rows : [];
  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs">
        {sql}
        {errorCode !== "" ? (
          <span className="text-destructive"> · 失败 {errorCode}</span>
        ) : null}
      </p>
      {columns.length > 0 ? (
        <Table>
          <TableHeader>
            <TableRow>
              {columns.map((column) => (
                <TableHead key={column}>{column}</TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => {
              const cells = Array.isArray(row) ? row : [];
              const text = cells.map((cell) =>
                typeof cell === "string" ? cell : "",
              );
              return (
                <TableRow key={text.join("\u0001")}>
                  {text.map((cell, cellIndex) => (
                    <TableCell
                      key={`${columns[cellIndex] ?? cellIndex}:${cell}`}
                      className="font-mono text-xs"
                    >
                      {cell}
                    </TableCell>
                  ))}
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      ) : null}
    </div>
  );
}
