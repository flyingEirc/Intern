import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { RestoreDetail } from "./restore-detail";

export const metadata: Metadata = { title: "恢复详情" };

export default function RestoreDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <RestoreDetail />
    </Suspense>
  );
}
