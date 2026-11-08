import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { BackupDetail } from "./backup-detail";

export const metadata: Metadata = { title: "备份详情" };

export default function BackupDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <BackupDetail />
    </Suspense>
  );
}
