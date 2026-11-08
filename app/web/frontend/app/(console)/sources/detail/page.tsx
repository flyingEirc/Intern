import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { SourceDetail } from "./source-detail";

export const metadata: Metadata = { title: "数据源详情" };

export default function SourceDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <SourceDetail />
    </Suspense>
  );
}
