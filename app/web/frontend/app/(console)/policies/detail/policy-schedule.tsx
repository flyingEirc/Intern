"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CalendarClockIcon, PlayIcon } from "lucide-react";
import { useRouter } from "next/navigation";
import { useRef, useState } from "react";
import { ResourceError, UnavailableAlert } from "@/components/resources/states";
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
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ApiError } from "@/lib/api/errors";
import { triggerPolicyRunMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import { formatZonedDateTime } from "@/lib/resources/format";
import { usePolicySchedule } from "@/lib/resources/queries";
import { runKindLabel } from "@/lib/resources/runs";
import { type TriggerAttempt, triggerAttempt } from "@/lib/runs/idempotency";

/**
 * The server-computed next fires, plus the manual trigger. The idempotency key
 * is minted per click and kept until that click settles, so the automatic retry
 * of a lost response and a double click both replay the same run.
 */
export function PolicySchedule({
  policyId,
  canTrigger,
}: {
  policyId: string;
  canTrigger: boolean;
}) {
  const router = useRouter();
  const queryClient = useQueryClient();
  const schedule = usePolicySchedule(policyId);
  const trigger = useMutation(triggerPolicyRunMutation());
  const [attempt, setAttempt] = useState<TriggerAttempt | null>(null);
  // The attempt is also kept in a ref: two activations dispatched before React
  // re-renders (a fast double click) both read the closure of the same render,
  // so state alone would mint a second key and create a second run.
  const attemptRef = useRef<TriggerAttempt | null>(null);

  function start(kind: TriggerAttempt["kind"]) {
    // A click that has not succeeded keeps its key, so pressing the same
    // button again retries that submission instead of creating a second run.
    // The other button stays disabled while one is in flight, so a key is
    // never reused for a different kind.
    const next = triggerAttempt(attemptRef.current, kind, () =>
      crypto.randomUUID(),
    );
    attemptRef.current = next;
    setAttempt(next);
    trigger.mutate(
      {
        path: { id: policyId },
        body: { kind },
        headers: { "Idempotency-Key": next.key },
      },
      {
        onSuccess: (accepted) => {
          attemptRef.current = null;
          setAttempt(null);
          queryClient.invalidateQueries();
          router.push(`/runs/detail?id=${accepted.run_id}`);
        },
      },
    );
  }

  const pendingKind = trigger.isPending ? attempt?.kind : undefined;
  const conflict =
    trigger.error instanceof ApiError && trigger.error.status === 409;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          <CalendarClockIcon className="size-4" />
          调度预览
          {schedule.data && !schedule.data.enabled ? (
            <Badge variant="outline">未启用</Badge>
          ) : null}
        </CardTitle>
        <CardDescription>
          下一次触发时间由服务端用调度器同一份 cron
          实现、按策略时区计算，页面不做推算。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {canTrigger ? (
          <div className="flex flex-wrap gap-2">
            <Button disabled={trigger.isPending} onClick={() => start("full")}>
              {pendingKind === "full" ? (
                <Spinner data-icon="inline-start" />
              ) : (
                <PlayIcon data-icon="inline-start" />
              )}
              立即全量
            </Button>
            <Button
              variant="outline"
              disabled={trigger.isPending}
              onClick={() => start("incremental")}
            >
              {pendingKind === "incremental" ? (
                <Spinner data-icon="inline-start" />
              ) : (
                <PlayIcon data-icon="inline-start" />
              )}
              立即增量
            </Button>
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            当前账号只有查看权限，不能手动触发运行。
          </p>
        )}

        {trigger.isError ? (
          trigger.error instanceof ApiError &&
          trigger.error.code === "unavailable" ? (
            <UnavailableAlert error={trigger.error} />
          ) : conflict ? (
            <Alert variant="destructive">
              <AlertTitle>无法触发</AlertTitle>
              <AlertDescription>
                策略未启用，或这次提交的幂等键已被一次不同的请求使用。未启用的策略不会产生运行。
              </AlertDescription>
            </Alert>
          ) : (
            <ResourceError title="触发失败" error={trigger.error} />
          )
        ) : null}

        {schedule.isPending ? <Skeleton className="h-24 w-full" /> : null}
        {schedule.isError ? (
          <ResourceError
            title="无法读取调度预览"
            error={schedule.error}
            onRetry={() => schedule.refetch()}
          />
        ) : null}
        {schedule.data ? (
          <>
            {!schedule.data.enabled ? (
              <p className="text-sm text-muted-foreground">
                策略未启用。下面列出的是启用后将会排定的触发时间，调度器目前不会执行它们。
              </p>
            ) : null}
            {schedule.data.fires.length === 0 ? (
              <p className="text-sm text-muted-foreground">没有排定的触发。</p>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>类型</TableHead>
                    <TableHead>触发时间</TableHead>
                    <TableHead>状态</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {schedule.data.fires.map((fire) => (
                    <TableRow key={`${fire.kind}:${fire.at}`}>
                      <TableCell>{runKindLabel[fire.kind]}</TableCell>
                      <TableCell>
                        {formatZonedDateTime(fire.at, schedule.data.timezone)}
                      </TableCell>
                      <TableCell>
                        {fire.missed ? (
                          <Badge variant="destructive">
                            已错过，下一次检查即执行
                          </Badge>
                        ) : (
                          <Badge variant="outline">按计划</Badge>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
