"use client";

import { ChevronRightIcon, MoreHorizontalIcon } from "lucide-react";
import Link from "next/link";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { cn } from "@/lib/utils";

export type RowAction = {
  label: string;
  href?: string;
  onClick?: () => void;
  destructive?: boolean;
  disabled?: boolean;
};

export function DataTableToolbar({
  title,
  description,
  count,
  search,
  onSearch,
  children,
}: {
  title: string;
  description?: string;
  count?: number;
  search?: string;
  onSearch?: (value: string) => void;
  children?: ReactNode;
}) {
  return (
    <CardHeader className="flex flex-col gap-4 border-b md:flex-row md:items-center md:justify-between">
      <div className="flex flex-col gap-1">
        <CardTitle className="flex flex-wrap items-center gap-2">
          {title}
          {count === undefined ? null : (
            <span className="text-xs font-normal text-muted-foreground">
              {count} 条
            </span>
          )}
        </CardTitle>
        {description ? <CardDescription>{description}</CardDescription> : null}
      </div>
      <div className="flex w-full flex-col gap-2 sm:w-auto sm:flex-row sm:items-center">
        {onSearch ? (
          <Field className="w-full sm:w-64">
            <FieldLabel className="sr-only" htmlFor={`${title}-filter`}>
              在本页查找
            </FieldLabel>
            <Input
              id={`${title}-filter`}
              value={search ?? ""}
              onChange={(event) => onSearch(event.target.value)}
              placeholder="在本页查找"
              autoComplete="off"
            />
            <FieldDescription className="sr-only">
              搜索当前显示的记录
            </FieldDescription>
          </Field>
        ) : null}
        {children ? (
          <div className="flex flex-wrap items-center gap-2">{children}</div>
        ) : null}
      </div>
    </CardHeader>
  );
}

export function DataPanel({
  title,
  description,
  count,
  search,
  onSearch,
  toolbar,
  children,
  footer,
  className,
}: {
  title: string;
  description?: string;
  count?: number;
  search?: string;
  onSearch?: (value: string) => void;
  toolbar?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  className?: string;
}) {
  return (
    <Card className={cn("overflow-hidden", className)}>
      <DataTableToolbar
        title={title}
        description={description}
        count={count}
        search={search}
        onSearch={onSearch}
      >
        {toolbar}
      </DataTableToolbar>
      <CardContent className="p-0">{children}</CardContent>
      {footer ? (
        <div className="flex flex-wrap items-center justify-between gap-2 border-t px-4 py-3">
          {footer}
        </div>
      ) : null}
    </Card>
  );
}

export function LoadMoreButton({
  hasNextPage,
  isFetching,
  onLoadMore,
}: {
  hasNextPage: boolean;
  isFetching: boolean;
  onLoadMore: () => void;
}) {
  if (!hasNextPage) {
    return null;
  }
  return (
    <Button variant="outline" disabled={isFetching} onClick={onLoadMore}>
      {isFetching ? <Spinner data-icon="inline-start" /> : null}
      加载更多
    </Button>
  );
}

export function RowActions({
  label,
  actions,
}: {
  label: string;
  actions: RowAction[];
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button variant="ghost" size="icon-sm" aria-label={`${label} 操作`} />
        }
      >
        <MoreHorizontalIcon />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-40">
        <DropdownMenuGroup>
          {actions.map((action) => (
            <DropdownMenuItem
              key={action.label}
              variant={action.destructive ? "destructive" : "default"}
              disabled={action.disabled}
              render={action.href ? <Link href={action.href} /> : undefined}
              onClick={action.onClick}
            >
              {action.label}
              <ChevronRightIcon data-icon="inline-end" />
            </DropdownMenuItem>
          ))}
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
