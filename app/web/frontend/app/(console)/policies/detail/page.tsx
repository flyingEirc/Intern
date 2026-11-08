import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { PolicyDetail } from "./policy-detail";

export const metadata: Metadata = { title: "策略详情" };

export default function PolicyDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <PolicyDetail />
    </Suspense>
  );
}
