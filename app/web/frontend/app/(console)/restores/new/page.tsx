import type { Metadata } from "next";
import { Suspense } from "react";
import { DetailSkeleton } from "@/components/resources/states";
import { RestoreWizard } from "./restore-wizard";

export const metadata: Metadata = { title: "新建恢复" };

export default function NewRestorePage() {
  return (
    <Suspense fallback={<DetailSkeleton />}>
      <RestoreWizard />
    </Suspense>
  );
}
