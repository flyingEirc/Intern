"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon, PauseIcon, PencilIcon, PlayIcon } from "lucide-react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useState } from "react";
import { DeleteResourceDialog } from "@/components/resources/delete-resource-dialog";
import { PreflightResultView } from "@/components/resources/preflight-result";
import {
  DetailSkeleton,
  PageHeader,
  ResourceError,
  UnavailableAlert,
} from "@/components/resources/states";
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
import { toast } from "@/components/ui/toast";
import { useSession } from "@/hooks/use-session";
import {
  deletePolicyMutation,
  disablePolicyMutation,
  enablePolicyMutation,
  getPolicyOptions,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import {
  describeResourceError,
  isConflict,
  isUnavailable,
  preflightOf,
} from "@/lib/resources/errors";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import {
  invalidatePolicies,
  useRepositoryChoices,
  useSourceChoices,
} from "@/lib/resources/queries";

export function PolicyDetail() {
  const id = useSearchParams().get("id");
  const router = useRouter();
  const queryClient = useQueryClient();
  const session = useSession();
  const [deleteError, setDeleteError] = useState<unknown>(null);
  const policy = useQuery({
    ...getPolicyOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
  const sources = useSourceChoices();
  const repositories = useRepositoryChoices();
  const enable = useMutation(enablePolicyMutation());
  const disable = useMutation(disablePolicyMutation());
  const remove = useMutation(deletePolicyMutation());
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.policies,
  );

  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少策略 ID</EmptyTitle>
          <EmptyDescription>
            请从策略列表打开详情；地址形如 /policies/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (policy.isPending) {
    return <DetailSkeleton />;
  }
  if (policy.isError) {
    return (
      <ResourceError
        title="无法读取策略"
        error={policy.error}
        onRetry={() => policy.refetch()}
      />
    );
  }

  const data = policy.data;
  const sourceName =
    sources.data?.items.find((item) => item.id === data.source_id)?.name ??
    data.source_id;
  const repositoryName = (repositoryId: string | null) =>
    repositoryId === null
      ? "—"
      : (repositories.data?.items.find((item) => item.id === repositoryId)
          ?.name ?? repositoryId);
  const enablePreflight = preflightOf(enable.error);

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={data.name}
        description="策略配置、启用状态与最近一次预检；启用前会运行真实预检。"
      >
        {canManage ? (
          <>
            <Button
              nativeButton={false}
              variant="outline"
              render={<Link href={`/policies/new?id=${data.id}`} />}
            >
              <PencilIcon data-icon="inline-start" />
              编辑
            </Button>
            {data.enabled ? (
              <Button
                variant="outline"
                disabled={disable.isPending}
                onClick={() =>
                  disable.mutate(
                    { path: { id: data.id } },
                    {
                      onSuccess: () => {
                        invalidatePolicies(queryClient, data.id);
                        toast.add({
                          type: "success",
                          title: "策略已停用",
                          description: data.name,
                        });
                      },
                    },
                  )
                }
              >
                {disable.isPending ? (
                  <Spinner data-icon="inline-start" />
                ) : (
                  <PauseIcon data-icon="inline-start" />
                )}
                停用
              </Button>
            ) : (
              <Button
                disabled={enable.isPending}
                onClick={() =>
                  enable.mutate(
                    { path: { id: data.id } },
                    {
                      onSuccess: () => {
                        invalidatePolicies(queryClient, data.id);
                        toast.add({
                          type: "success",
                          title: "策略已启用",
                          description: data.name,
                        });
                      },
                      onError: () => invalidatePolicies(queryClient, data.id),
                    },
                  )
                }
              >
                {enable.isPending ? (
                  <Spinner data-icon="inline-start" />
                ) : (
                  <PlayIcon data-icon="inline-start" />
                )}
                启用
              </Button>
            )}
            <DeleteResourceDialog
              resourceLabel="策略"
              name={data.name}
              isPending={remove.isPending}
              onConfirm={() =>
                remove.mutate(
                  { path: { id: data.id } },
                  {
                    onSuccess: () => {
                      invalidatePolicies(queryClient);
                      toast.add({
                        type: "success",
                        title: "策略已删除",
                        description: data.name,
                      });
                      router.push("/policies");
                    },
                    onError: (error) => setDeleteError(error),
                  },
                )
              }
            />
          </>
        ) : null}
      </PageHeader>

      {enablePreflight ? (
        <div className="flex flex-col gap-2">
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>启用失败：预检未通过</AlertTitle>
            <AlertDescription>
              策略保持未启用，服务端没有写入启用状态。
            </AlertDescription>
          </Alert>
          <PreflightResultView result={enablePreflight} title="启用预检结果" />
        </div>
      ) : null}
      {enable.isError && !enablePreflight ? (
        isUnavailable(enable.error) ? (
          <UnavailableAlert error={enable.error} />
        ) : (
          <ResourceError title="启用失败" error={enable.error} />
        )
      ) : null}
      {disable.isError ? (
        <ResourceError title="停用失败" error={disable.error} />
      ) : null}
      {deleteError ? (
        isConflict(deleteError) ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>无法删除：策略仍被使用</AlertTitle>
            <AlertDescription>
              {describeResourceError(deleteError)}
            </AlertDescription>
          </Alert>
        ) : (
          <ResourceError title="删除失败" error={deleteError} />
        )
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2">
            {data.name}
            <Badge variant={data.enabled ? "secondary" : "outline"}>
              {data.enabled ? "已启用" : "未启用"}
            </Badge>
          </CardTitle>
          <CardDescription>策略 {data.id}</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">数据源</dt>
            <dd>{sourceName}</dd>
            <dt className="text-muted-foreground">本地仓库</dt>
            <dd>{repositoryName(data.local_repository_id)}</dd>
            <dt className="text-muted-foreground">副本仓库</dt>
            <dd>{repositoryName(data.replica_repository_id)}</dd>
            <dt className="text-muted-foreground">全量 cron</dt>
            <dd className="font-mono">{data.full_cron}</dd>
            <dt className="text-muted-foreground">增量 cron</dt>
            <dd className="font-mono">{data.incremental_cron}</dd>
            <dt className="text-muted-foreground">时区</dt>
            <dd className="font-mono">{data.timezone}</dd>
            <dt className="text-muted-foreground">最大链长</dt>
            <dd>{data.max_chain_length}</dd>
            <dt className="text-muted-foreground">并行度</dt>
            <dd>{data.parallel}</dd>
            <dt className="text-muted-foreground">限速</dt>
            <dd>{data.throttle} 字节/秒</dd>
            <dt className="text-muted-foreground">zstd 级别</dt>
            <dd>{data.zstd_level}</dd>
            <dt className="text-muted-foreground">page tracking</dt>
            <dd>{data.page_tracking ? "启用" : "关闭"}</dd>
            <dt className="text-muted-foreground">创建时间</dt>
            <dd>{formatDateTime(data.created_at)}</dd>
            <dt className="text-muted-foreground">更新时间</dt>
            <dd>{formatDateTime(data.updated_at)}</dd>
          </dl>
        </CardContent>
      </Card>

      {data.last_preflight ? (
        <PreflightResultView
          result={data.last_preflight}
          title="最近一次预检"
        />
      ) : (
        <Card>
          <CardHeader>
            <CardTitle>最近一次预检</CardTitle>
            <CardDescription>还没有预检记录。</CardDescription>
          </CardHeader>
        </Card>
      )}
    </div>
  );
}
