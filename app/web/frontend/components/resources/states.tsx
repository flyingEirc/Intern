"use client";

import { CircleAlertIcon, LockIcon, TriangleAlertIcon } from "lucide-react";
import type { ReactNode } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { describeResourceError, isUnavailable } from "@/lib/resources/errors";

export function PageHeader({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="flex flex-col gap-1">
        <h2 className="font-heading text-lg font-medium">{title}</h2>
        {description ? (
          <p className="text-sm text-muted-foreground">{description}</p>
        ) : null}
      </div>
      {children ? (
        <div className="flex flex-wrap items-center gap-2">{children}</div>
      ) : null}
    </div>
  );
}

export function NoPermission({ action }: { action: string }) {
  return (
    <Empty className="border">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <LockIcon />
        </EmptyMedia>
        <EmptyTitle>没有权限</EmptyTitle>
        <EmptyDescription>
          当前账号没有{action}
          的权限。页面可以查看，但写操作只对具备相应管理权限的角色开放。
        </EmptyDescription>
      </EmptyHeader>
    </Empty>
  );
}

export function ResourceError({
  title = "加载失败",
  error,
  onRetry,
}: {
  title?: string;
  error: unknown;
  onRetry?: () => void;
}) {
  if (isUnavailable(error)) {
    return <UnavailableAlert error={error} />;
  }
  return (
    <Alert variant="destructive">
      <CircleAlertIcon />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription className="flex flex-col items-start gap-3">
        {describeResourceError(error)}
        {onRetry ? (
          <Button variant="outline" size="sm" onClick={onRetry}>
            重试
          </Button>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

/** Fixed explanation for `503 unavailable`; the backend message is appended. */
export function UnavailableAlert({ error }: { error: unknown }) {
  return (
    <Alert variant="destructive">
      <TriangleAlertIcon />
      <AlertTitle>部署缺少必要配置</AlertTitle>
      <AlertDescription>{describeResourceError(error)}</AlertDescription>
    </Alert>
  );
}

export function ListSkeleton() {
  return (
    <div className="flex flex-col gap-2" aria-busy="true">
      <span className="sr-only">正在加载</span>
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
      <Skeleton className="h-10 w-full" />
    </div>
  );
}

export function DetailSkeleton() {
  return (
    <div className="flex flex-col gap-4" aria-busy="true">
      <span className="sr-only">正在加载</span>
      <Skeleton className="h-8 w-56" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}
