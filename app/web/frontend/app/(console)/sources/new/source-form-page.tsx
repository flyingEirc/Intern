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
import { getSourceOptions } from "@/lib/api/generated/@tanstack/react-query.gen";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import { SourceForm } from "../source-form";

export function SourceFormPage() {
  const id = useSearchParams().get("id");
  const session = useSession();
  const source = useQuery({
    ...getSourceOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.sources,
  );

  if (!canManage) {
    return <NoPermission action="管理数据源" />;
  }
  if (id !== null && source.isPending) {
    return <DetailSkeleton />;
  }
  if (id !== null && source.isError) {
    return (
      <ResourceError
        title="无法读取数据源"
        error={source.error}
        onRetry={() => source.refetch()}
      />
    );
  }
  const editing = source.data;
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={editing ? `编辑数据源：${editing.name}` : "新建数据源"}
        description={
          editing
            ? "密码留空表示保留原值；服务器身份由预检刷新。"
            : "填写 intern 容器内可见的 socket、datadir 与只写密码。"
        }
      />
      <SourceForm source={editing} />
    </div>
  );
}
