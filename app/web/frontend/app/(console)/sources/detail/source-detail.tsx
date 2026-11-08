"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon, PencilIcon, PlayIcon } from "lucide-react";
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
import { toast } from "@/components/ui/toast";
import { useSession } from "@/hooks/use-session";
import {
  deleteSourceMutation,
  getSourceOptions,
  preflightSourceMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import {
  describeResourceError,
  isConflict,
  isUnavailable,
} from "@/lib/resources/errors";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import {
  invalidateSources,
  useRepositoryChoices,
} from "@/lib/resources/queries";
import { SourceProtection } from "./source-protection";

export function SourceDetail() {
  const id = useSearchParams().get("id");
  const router = useRouter();
  const queryClient = useQueryClient();
  const session = useSession();
  const [localRepositoryId, setLocalRepositoryId] = useState("");
  const [deleteError, setDeleteError] = useState<unknown>(null);
  const source = useQuery({
    ...getSourceOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
  const repositories = useRepositoryChoices();
  const preflight = useMutation(preflightSourceMutation());
  const remove = useMutation(deleteSourceMutation());
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.sources,
  );
  const canManageRepositories = hasPermission(
    session.data?.permissions,
    managePermission.repositories,
  );
  const localRepositories =
    repositories.data?.items.filter((item) => item.kind === "local") ?? [];
  const selectedRepository =
    localRepositoryId || (localRepositories[0]?.id ?? "");
  // Base UI shows the raw value in a closed trigger unless the labels are known.
  const repositoryLabels = Object.fromEntries(
    localRepositories.map((repository) => [repository.id, repository.name]),
  );

  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少数据源 ID</EmptyTitle>
          <EmptyDescription>
            请从数据源列表打开详情；地址形如 /sources/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (source.isPending) {
    return <DetailSkeleton />;
  }
  if (source.isError) {
    return (
      <ResourceError
        title="无法读取数据源"
        error={source.error}
        onRetry={() => source.refetch()}
      />
    );
  }

  const data = source.data;
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={data.name}
        description="数据源配置、服务器身份与真实预检；已保存的密码不会显示。"
      >
        {canManage ? (
          <>
            <Button
              nativeButton={false}
              variant="outline"
              render={<Link href={`/sources/new?id=${data.id}`} />}
            >
              <PencilIcon data-icon="inline-start" />
              编辑
            </Button>
            <DeleteResourceDialog
              resourceLabel="数据源"
              name={data.name}
              isPending={remove.isPending}
              onConfirm={() =>
                remove.mutate(
                  { path: { id: data.id } },
                  {
                    onSuccess: () => {
                      invalidateSources(queryClient);
                      toast.add({
                        type: "success",
                        title: "数据源已删除",
                        description: data.name,
                      });
                      router.push("/sources");
                    },
                    onError: (error) => setDeleteError(error),
                  },
                )
              }
            />
          </>
        ) : null}
      </PageHeader>

      {deleteError ? (
        isConflict(deleteError) ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>无法删除：数据源仍被使用</AlertTitle>
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
          <CardTitle>{data.name}</CardTitle>
          <CardDescription>数据源 {data.id}</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">Socket</dt>
            <dd className="font-mono">{data.socket}</dd>
            <dt className="text-muted-foreground">Datadir</dt>
            <dd className="font-mono">{data.datadir}</dd>
            <dt className="text-muted-foreground">用户名</dt>
            <dd>{data.username}</dd>
            <dt className="text-muted-foreground">密码</dt>
            <dd>{data.has_secret ? "已设置（不会显示）" : "未设置"}</dd>
            <dt className="text-muted-foreground">服务器 UUID</dt>
            <dd className="font-mono">{data.server_uuid ?? "—"}</dd>
            <dt className="text-muted-foreground">服务器版本</dt>
            <dd>{data.server_version ?? "—"}</dd>
            <dt className="text-muted-foreground">创建时间</dt>
            <dd>{formatDateTime(data.created_at)}</dd>
            <dt className="text-muted-foreground">更新时间</dt>
            <dd>{formatDateTime(data.updated_at)}</dd>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex flex-wrap items-center gap-2">
            预检
            {data.last_preflight ? (
              <Badge
                variant={data.last_preflight.ok ? "secondary" : "destructive"}
              >
                {data.last_preflight.ok ? "上次通过" : "上次未通过"}
              </Badge>
            ) : (
              <Badge variant="outline">尚未预检</Badge>
            )}
          </CardTitle>
          <CardDescription>
            预检以本地仓库为备份根目录查询真实
            MySQL；成功会记录服务器身份与预检结果。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {canManage ? (
            localRepositories.length === 0 ? (
              <Alert>
                <CircleAlertIcon />
                <AlertTitle>需要至少一个本地仓库</AlertTitle>
                <AlertDescription className="flex flex-col items-start gap-2">
                  预检需要把本地仓库作为备份根目录。
                  {canManageRepositories ? (
                    <Button
                      nativeButton={false}
                      variant="outline"
                      size="sm"
                      render={<Link href="/repositories/new" />}
                    >
                      新建本地仓库
                    </Button>
                  ) : null}
                </AlertDescription>
              </Alert>
            ) : (
              <div className="flex flex-wrap items-end gap-2">
                <Field className="w-full max-w-sm">
                  <FieldLabel htmlFor="preflight-repository">
                    本地仓库
                  </FieldLabel>
                  <Select
                    items={repositoryLabels}
                    value={selectedRepository}
                    onValueChange={(value) =>
                      setLocalRepositoryId(value as string)
                    }
                  >
                    <SelectTrigger id="preflight-repository" className="w-full">
                      <SelectValue placeholder="选择本地仓库" />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectGroup>
                        {localRepositories.map((repository) => (
                          <SelectItem key={repository.id} value={repository.id}>
                            {repository.name}
                          </SelectItem>
                        ))}
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                </Field>
                <Button
                  disabled={preflight.isPending || selectedRepository === ""}
                  onClick={() =>
                    preflight.mutate(
                      {
                        path: { id: data.id },
                        body: { local_repository_id: selectedRepository },
                      },
                      {
                        onSuccess: () =>
                          invalidateSources(queryClient, data.id),
                      },
                    )
                  }
                >
                  {preflight.isPending ? (
                    <Spinner data-icon="inline-start" />
                  ) : (
                    <PlayIcon data-icon="inline-start" />
                  )}
                  运行预检
                </Button>
              </div>
            )
          ) : (
            <p className="text-sm text-muted-foreground">
              只有具备数据源管理权限的账号可以运行预检。
            </p>
          )}

          {preflight.isError ? (
            isUnavailable(preflight.error) ? (
              <UnavailableAlert error={preflight.error} />
            ) : (
              <ResourceError title="预检失败" error={preflight.error} />
            )
          ) : null}
          {preflight.data ? (
            <PreflightResultView result={preflight.data} title="本次预检结果" />
          ) : data.last_preflight ? (
            <PreflightResultView
              result={data.last_preflight}
              title="最近一次预检"
            />
          ) : (
            <p className="text-sm text-muted-foreground">还没有预检记录。</p>
          )}
        </CardContent>
      </Card>

      <SourceProtection sourceId={data.id} />
    </div>
  );
}
