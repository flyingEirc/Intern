import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { RunDetail } from "./run-detail";

export const metadata: Metadata = { title: "运行详情" };

export default function RunDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <RunDetail />
    </Suspense>
  );
}
