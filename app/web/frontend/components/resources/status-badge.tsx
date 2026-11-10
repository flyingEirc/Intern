"use client";

import type { ReactNode } from "react";
import { Badge } from "@/components/ui/badge";

export type StatusTone = "success" | "danger" | "active" | "neutral";

const toneVariant: Record<
  StatusTone,
  React.ComponentProps<typeof Badge>["variant"]
> = {
  success: "success",
  danger: "destructive",
  active: "default",
  neutral: "outline",
};

/** One status-to-color mapping shared by every console list and detail. */
export function StatusBadge({
  tone,
  children,
}: {
  tone: StatusTone;
  children: ReactNode;
}) {
  return <Badge variant={toneVariant[tone]}>{children}</Badge>;
}

export function runStatusTone(status: string): StatusTone {
  if (status === "completed") return "success";
  if (status === "failed") return "danger";
  if (status === "running") return "active";
  return "neutral";
}

export function restoreStatusTone(status: string): StatusTone {
  if (status === "completed") return "success";
  if (status === "failed") return "danger";
  if (status === "running") return "active";
  return "neutral";
}

export function replicaStatusTone(status: string): StatusTone {
  if (status === "verified") return "success";
  if (status === "failed" || status === "exhausted") return "danger";
  if (status === "uploading") return "active";
  return "neutral";
}
