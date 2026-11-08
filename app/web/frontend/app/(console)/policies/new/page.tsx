import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { PolicyFormPage } from "./policy-form-page";

export const metadata: Metadata = { title: "新建策略" };

export default function NewPolicyPage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <PolicyFormPage />
    </Suspense>
  );
}
