"use client";

import type { LucideIcon } from "lucide-react";
import Link from "next/link";
import {
  Card,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

export function MetricCard({
  label,
  value,
  detail,
  icon: Icon,
  href,
  pending,
}: {
  label: string;
  value: string;
  detail: string;
  icon: LucideIcon;
  href?: string;
  pending?: boolean;
}) {
  const content = (
    <Card className="h-full transition-colors hover:bg-muted/40">
      <CardHeader className="flex flex-row items-start justify-between gap-2">
        <div className="flex flex-col gap-1">
          <CardDescription>{label}</CardDescription>
          <CardTitle className="text-2xl font-semibold tabular-nums">
            {pending ? <Skeleton className="h-8 w-16" /> : value}
          </CardTitle>
          <p className="text-xs text-muted-foreground">{detail}</p>
        </div>
        <span className="hidden size-9 shrink-0 items-center justify-center rounded-lg bg-muted sm:flex">
          <Icon />
        </span>
      </CardHeader>
    </Card>
  );
  return href ? (
    <Link
      href={href}
      className="rounded-xl focus-visible:outline-2 focus-visible:outline-ring focus-visible:outline-offset-2"
    >
      {content}
    </Link>
  ) : (
    content
  );
}
