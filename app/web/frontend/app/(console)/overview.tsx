"use client";

import { useQuery } from "@tanstack/react-query";
import {
  CalendarClockIcon,
  DatabaseIcon,
  HardDriveIcon,
  HistoryIcon,
  PlusIcon,
  ShieldCheckIcon,
} from "lucide-react";
import Link from "next/link";
import { MetricCard } from "@/components/resources/metric-card";
import { PageHeader, ResourceError } from "@/components/resources/states";
import {
  runStatusTone,
  StatusBadge,
} from "@/components/resources/status-badge";
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
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useSession } from "@/hooks/use-session";
import { readinessOptions } from "@/lib/api/generated/@tanstack/react-query.gen";
import { describeError } from "@/lib/api/messages";
import { formatDateTime } from "@/lib/resources/format";
import { hasLocalRepository } from "@/lib/resources/onboarding";
import { hasPermission } from "@/lib/resources/permissions";
import {
  usePolicyChoices,
  useRepositoryChoices,
  useRestorePages,
  useRunPages,
  useSourceChoices,
} from "@/lib/resources/queries";
import { runStatusLabel } from "@/lib/resources/runs";

function Readiness() {
  const readiness = useQuery({
    ...readinessOptions(),
    refetchInterval: 30_000,
    retry: false,
  });
  return (
    <Card>
      <CardHeader>
        <CardTitle>服务状态</CardTitle>
        <CardDescription>
          {readiness.isPending
            ? "正在检查系统运行状态"
            : readiness.isError
              ? "系统运行异常"
              : "系统运行正常"}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-2">
        {readiness.isPending ? (
          <Skeleton className="h-6 w-20" />
        ) : readiness.isError ? (
          <>
            <StatusBadge tone="danger">未就绪</StatusBadge>
            <p className="text-sm text-muted-foreground">
              {describeError(readiness.error)}
            </p>
          </>
        ) : (
          <StatusBadge tone="success">就绪</StatusBadge>
        )}
      </CardContent>
    </Card>
  );
}

export function Overview() {
  const { data } = useSession();
  const policies = usePolicyChoices();
  const sources = useSourceChoices();
  const repositories = useRepositoryChoices();
  const runs = useRunPages({});
  const restores = useRestorePages({});

  if (!data) {
    return null;
  }

  const policyItems = policies.data?.items ?? [];
  const policyNames = new Map(
    policyItems.map((policy) => [policy.id, policy.name]),
  );
  const enabledPolicies = policyItems.filter((item) => item.enabled).length;
  const runItems = runs.data?.pages.flatMap((page) => page.items) ?? [];
  const restoreItems = restores.data?.pages.flatMap((page) => page.items) ?? [];
  const countLabel = (count: number, hasMore: boolean) =>
    `${count}${hasMore ? "+" : ""}`;
  const canManageSources = hasPermission(data.permissions, "sources:manage");
  const canManageRepositories = hasPermission(
    data.permissions,
    "repositories:manage",
  );
  const canManagePolicies = hasPermission(data.permissions, "policies:manage");
  const storageReady = hasLocalRepository(repositories.data?.items ?? []);
  const canRestore = hasPermission(data.permissions, "restores:plan");
  const hasRestorableBackup = runItems.some(
    (item) => item.status === "completed" && item.backup_id !== null,
  );
  const needsSetup =
    sources.isSuccess &&
    repositories.isSuccess &&
    policies.isSuccess &&
    ((sources.data?.items.length ?? 0) === 0 ||
      !storageReady ||
      policyItems.length === 0);

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="数据保护概览"
        description="查看数据库保护状态，跟踪备份任务与恢复进度。"
      >
        {canManagePolicies ? (
          <Button nativeButton={false} render={<Link href="/policies/new" />}>
            <PlusIcon data-icon="inline-start" />
            新建保护策略
          </Button>
        ) : null}
        {canRestore ? (
          hasRestorableBackup ? (
            <Button
              nativeButton={false}
              variant="outline"
              render={<Link href="/restores/new" />}
            >
              <HistoryIcon data-icon="inline-start" />
              新建恢复
            </Button>
          ) : (
            <Tooltip>
              <TooltipTrigger render={<span className="inline-flex" />}>
                <Button disabled variant="outline">
                  <HistoryIcon data-icon="inline-start" />
                  新建恢复
                </Button>
              </TooltipTrigger>
              <TooltipContent>还没有可恢复的备份</TooltipContent>
            </Tooltip>
          )
        ) : null}
      </PageHeader>

      <section className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <MetricCard
          label="保护策略"
          value={countLabel(
            policyItems.length,
            Boolean(policies.data?.next_before),
          )}
          detail={`${enabledPolicies}${policies.data?.next_before ? "+" : ""} 项已启用`}
          icon={CalendarClockIcon}
          href="/policies"
          pending={policies.isPending}
        />
        <MetricCard
          label="数据源"
          value={countLabel(
            sources.data?.items.length ?? 0,
            Boolean(sources.data?.next_before),
          )}
          detail="已添加的数据库"
          icon={DatabaseIcon}
          href="/sources"
          pending={sources.isPending}
        />
        <MetricCard
          label="备份仓库"
          value={countLabel(
            repositories.data?.items.length ?? 0,
            Boolean(repositories.data?.next_before),
          )}
          detail="本地与异地存储"
          icon={HardDriveIcon}
          href="/repositories"
          pending={repositories.isPending}
        />
        <MetricCard
          label="恢复任务"
          value={countLabel(restoreItems.length, restores.hasNextPage)}
          detail="隔离恢复与演练"
          icon={ShieldCheckIcon}
          href="/restores"
          pending={runs.isPending || restores.isPending}
        />
      </section>
      {needsSetup ? (
        <Card>
          <CardHeader>
            <CardTitle>开始保护数据库</CardTitle>
            <CardDescription>
              先连接数据库并准备存储，再安排自动备份。
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3 md:grid-cols-3">
            {[
              {
                title: "1 · 添加数据源",
                detail: "选择要保护的 MySQL 数据库",
                href: "/sources/new",
                allowed: canManageSources,
                done: (sources.data?.items.length ?? 0) > 0,
                action: "添加数据源",
              },
              {
                title: "2 · 配置备份仓库",
                detail: "设置本地备份与异地存储",
                href: "/repositories/new",
                allowed: canManageRepositories,
                done: storageReady,
                action: "新建仓库",
              },
              {
                title: "3 · 创建保护策略",
                detail: "安排全量与增量备份",
                href: "/policies/new",
                allowed: canManagePolicies,
                done: policyItems.length > 0,
                action: "创建保护策略",
              },
            ].map((step) => (
              <div
                key={step.href}
                className="flex flex-col items-start gap-2 rounded-lg border p-4"
              >
                <p className="font-medium">{step.title}</p>
                <p className="text-sm text-muted-foreground">{step.detail}</p>
                {step.done ? (
                  <Badge variant="secondary">已完成</Badge>
                ) : step.allowed ? (
                  <Button
                    nativeButton={false}
                    variant="outline"
                    render={<Link href={step.href} />}
                  >
                    {step.action}
                  </Button>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    请联系管理员配置
                  </p>
                )}
              </div>
            ))}
          </CardContent>
        </Card>
      ) : null}

      <section className="grid items-start gap-4 xl:grid-cols-3">
        <Card className="xl:col-span-2">
          <CardHeader>
            <CardTitle>最近运行</CardTitle>
            <CardDescription>最新创建的全量与增量保护任务</CardDescription>
          </CardHeader>
          <CardContent>
            {runs.isPending ? <Skeleton className="h-32 w-full" /> : null}
            {runs.isError ? (
              <ResourceError
                title="无法读取运行"
                error={runs.error}
                onRetry={() => runs.refetch()}
              />
            ) : null}
            {runs.isSuccess && runItems.length === 0 ? (
              <Empty className="min-h-32 border-0 p-4">
                <EmptyHeader>
                  <EmptyMedia variant="icon">
                    <HistoryIcon />
                  </EmptyMedia>
                  <EmptyTitle className="text-sm">还没有运行记录</EmptyTitle>
                  <EmptyDescription>
                    创建并启用保护策略后，备份会自动运行。
                  </EmptyDescription>
                </EmptyHeader>
              </Empty>
            ) : null}
            {runItems.length > 0 ? (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>状态</TableHead>
                    <TableHead>类型</TableHead>
                    <TableHead>策略</TableHead>
                    <TableHead>创建时间</TableHead>
                    <TableHead>操作</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {runItems.slice(0, 5).map((run) => (
                    <TableRow key={run.id}>
                      <TableCell>
                        <StatusBadge tone={runStatusTone(run.status)}>
                          {runStatusLabel[run.status]}
                        </StatusBadge>
                      </TableCell>
                      <TableCell>
                        {run.kind === "full" ? "全量" : "增量"}
                      </TableCell>
                      <TableCell className="truncate">
                        {policyNames.get(run.policy_id) ?? "策略已删除"}
                      </TableCell>
                      <TableCell>{formatDateTime(run.created_at)}</TableCell>
                      <TableCell>
                        <Button
                          nativeButton={false}
                          variant="ghost"
                          size="sm"
                          render={<Link href={`/runs/detail?id=${run.id}`} />}
                        >
                          查看
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            ) : null}
          </CardContent>
        </Card>

        <div className="flex flex-col gap-4">
          <Readiness />
          {needsSetup ? null : (
            <Card>
              <CardHeader>
                <CardTitle>接下来</CardTitle>
                <CardDescription>检查保护安排或验证恢复能力</CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col items-start gap-3">
                <Button
                  nativeButton={false}
                  variant="outline"
                  render={<Link href="/policies" />}
                >
                  查看保护安排
                </Button>
                <Button
                  nativeButton={false}
                  variant="outline"
                  render={<Link href="/restores" />}
                >
                  查看恢复与演练
                </Button>
              </CardContent>
            </Card>
          )}
        </div>
      </section>
    </div>
  );
}
