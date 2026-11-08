"use client";

import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "next/navigation";
import {
  DetailSkeleton,
  NoPermission,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { useSession } from "@/hooks/use-session";
import { getRepositoryOptions } from "@/lib/api/generated/@tanstack/react-query.gen";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import { RepositoryForm } from "../repository-form";

export function RepositoryFormPage() {
  const id = useSearchParams().get("id");
  const session = useSession();
  const repository = useQuery({
    ...getRepositoryOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.repositories,
  );

  if (!canManage) {
    return <NoPermission action="管理备份仓库" />;
  }
  if (id !== null && repository.isPending) {
    return <DetailSkeleton />;
  }
  if (id !== null && repository.isError) {
    return (
      <ResourceError
        title="无法读取仓库"
        error={repository.error}
        onRetry={() => repository.refetch()}
      />
    );
  }
  const editing = repository.data;
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={editing ? `编辑仓库：${editing.name}` : "新建仓库"}
        description={
          editing
            ? "名称与 FTP 地址可以修改；类型和传输方式不可更改，密码留空表示保留原值。"
            : "本地目录要求绝对路径；FTP 仓库必须显式选择明文、显式 TLS 或隐式 TLS。"
        }
      />
      <RepositoryForm repository={editing} />
    </div>
  );
}
