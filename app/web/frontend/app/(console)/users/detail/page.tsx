import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { UserDetail } from "./user-detail";

export const metadata: Metadata = { title: "用户详情" };

export default function UserDetailPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <UserDetail />
    </Suspense>
  );
}
