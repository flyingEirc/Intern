"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CircleAlertIcon,
  CircleCheckIcon,
  PencilIcon,
  PlugZapIcon,
} from "lucide-react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useState } from "react";
import { DeleteResourceDialog } from "@/components/resources/delete-resource-dialog";
import {
  PlaintextBadge,
  PlaintextWarning,
} from "@/components/resources/plaintext-warning";
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
  deleteRepositoryMutation,
  getRepositoryOptions,
  probeRepositoryMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import {
  describeResourceError,
  isConflict,
  isUnavailable,
} from "@/lib/resources/errors";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import { invalidateRepositories } from "@/lib/resources/queries";
import {
  repositoryEndpoint,
  repositoryKindLabel,
  tlsModeLabel,
} from "@/lib/resources/repository";

export function RepositoryDetail() {
  const id = useSearchParams().get("id");
  const router = useRouter();
  const queryClient = useQueryClient();
  const session = useSession();
  const [deleteError, setDeleteError] = useState<unknown>(null);
  const repository = useQuery({
    ...getRepositoryOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
  const probe = useMutation(probeRepositoryMutation());
  const remove = useMutation(deleteRepositoryMutation());
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.repositories,
  );

  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少仓库 ID</EmptyTitle>
          <EmptyDescription>
            请从仓库列表打开详情；地址形如 /repositories/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (repository.isPending) {
    return <DetailSkeleton />;
  }
  if (repository.isError) {
    return (
      <ResourceError
        title="无法读取仓库"
        error={repository.error}
        onRetry={() => repository.refetch()}
      />
    );
  }

  const data = repository.data;
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={data.name}
        description="仓库配置与写入性探测；已保存的密码不会显示。"
      >
        {canManage ? (
          <>
            <Button
              nativeButton={false}
              variant="outline"
              render={<Link href={`/repositories/new?id=${data.id}`} />}
            >
              <PencilIcon data-icon="inline-start" />
              编辑
            </Button>
            <DeleteResourceDialog
              resourceLabel="仓库"
              name={data.name}
              isPending={remove.isPending}
              onConfirm={() =>
                remove.mutate(
                  { path: { id: data.id } },
                  {
                    onSuccess: () => {
                      invalidateRepositories(queryClient);
                      toast.add({
                        type: "success",
                        title: "仓库已删除",
                        description: data.name,
                      });
                      router.push("/repositories");
                    },
                    onError: (error) => setDeleteError(error),
                  },
                )
              }
            />
          </>
        ) : null}
      </PageHeader>

      {data.plaintext_credentials ? <PlaintextWarning /> : null}

      {deleteError ? (
        isConflict(deleteError) ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>无法删除：仓库仍被使用</AlertTitle>
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
            {data.plaintext_credentials ? <PlaintextBadge /> : null}
          </CardTitle>
          <CardDescription>仓库 {data.id}</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">类型</dt>
            <dd>
              <Badge variant="outline">{repositoryKindLabel[data.kind]}</Badge>
            </dd>
            <dt className="text-muted-foreground">传输方式</dt>
            <dd>{tlsModeLabel[data.tls_mode]}</dd>
            <dt className="text-muted-foreground">位置</dt>
            <dd className="font-mono">{repositoryEndpoint(data)}</dd>
            {data.ftp ? (
              <>
                <dt className="text-muted-foreground">FTP 用户</dt>
                <dd>{data.ftp.username}</dd>
                <dt className="text-muted-foreground">CA bundle</dt>
                <dd>
                  {data.tls_mode === "plain"
                    ? "不适用（明文）"
                    : data.ftp.ca_pem
                      ? "已配置"
                      : "使用系统根证书"}
                </dd>
              </>
            ) : null}
            <dt className="text-muted-foreground">密码</dt>
            <dd>{data.has_secret ? "已设置（不会显示）" : "未设置"}</dd>
            <dt className="text-muted-foreground">创建时间</dt>
            <dd>{formatDateTime(data.created_at)}</dd>
            <dt className="text-muted-foreground">更新时间</dt>
            <dd>{formatDateTime(data.updated_at)}</dd>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>写入性探测</CardTitle>
          <CardDescription>
            本地仓库通过私有暂存写入并删除来验证；FTP
            仓库按配置的传输方式打开但不发布。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          {canManage ? (
            <div>
              <Button
                variant="outline"
                disabled={probe.isPending}
                onClick={() => probe.mutate({ path: { id: data.id } })}
              >
                {probe.isPending ? (
                  <Spinner data-icon="inline-start" />
                ) : (
                  <PlugZapIcon data-icon="inline-start" />
                )}
                运行探测
              </Button>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">
              只有具备仓库管理权限的账号可以运行探测。
            </p>
          )}

          {probe.data?.ok ? (
            <Alert>
              <CircleCheckIcon />
              <AlertTitle>探测通过</AlertTitle>
              <AlertDescription>
                仓库可写，且探测没有发布任何内容。
              </AlertDescription>
            </Alert>
          ) : null}
          {probe.data && !probe.data.ok ? (
            <Alert variant="destructive">
              <CircleAlertIcon />
              <AlertTitle>探测未通过</AlertTitle>
              <AlertDescription>
                {probe.data.error
                  ? `错误码 ${probe.data.error.code}（FTP 应答 ${probe.data.error.reply_code}）`
                  : "服务返回 ok=false。"}
              </AlertDescription>
            </Alert>
          ) : null}
          {probe.isError ? (
            isUnavailable(probe.error) ? (
              <UnavailableAlert error={probe.error} />
            ) : (
              <ResourceError title="探测失败" error={probe.error} />
            )
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}
