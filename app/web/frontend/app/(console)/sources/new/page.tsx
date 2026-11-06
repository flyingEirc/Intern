import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { SourceFormPage } from "./source-form-page";

export const metadata: Metadata = { title: "新建数据源" };

export default function NewSourcePage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <SourceFormPage />
    </Suspense>
  );
}
