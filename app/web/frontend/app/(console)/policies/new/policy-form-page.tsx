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
import { getPolicyOptions } from "@/lib/api/generated/@tanstack/react-query.gen";
import { hasPermission, managePermission } from "@/lib/resources/permissions";
import { PolicyForm } from "../policy-form";

export function PolicyFormPage() {
  const id = useSearchParams().get("id");
  const session = useSession();
  const policy = useQuery({
    ...getPolicyOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
  const canManage = hasPermission(
    session.data?.permissions,
    managePermission.policies,
  );

  if (!canManage) {
    return <NoPermission action="管理备份策略" />;
  }
  if (id !== null && policy.isPending) {
    return <DetailSkeleton />;
  }
  if (id !== null && policy.isError) {
    return (
      <ResourceError
        title="无法读取策略"
        error={policy.error}
        onRetry={() => policy.refetch()}
      />
    );
  }
  const editing = policy.data;
  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={editing ? `编辑策略：${editing.name}` : "新建策略"}
        description={
          editing
            ? "更新保护安排，启用中的策略会检查连接与备份环境。"
            : "选择保护目标并安排自动备份。"
        }
      />
      <PolicyForm policy={editing} />
    </div>
  );
}
