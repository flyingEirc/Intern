import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { RepositoryFormPage } from "./repository-form-page";

export const metadata: Metadata = { title: "新建仓库" };

export default function NewRepositoryPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <RepositoryFormPage />
    </Suspense>
  );
}
