import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { RepositoryDetail } from "./repository-detail";

export const metadata: Metadata = { title: "仓库详情" };

export default function RepositoryDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <RepositoryDetail />
    </Suspense>
  );
}
