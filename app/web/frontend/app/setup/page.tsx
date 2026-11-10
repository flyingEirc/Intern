import type { Metadata } from "next";
import { Suspense } from "react";
import { SetupForm } from "./setup-form";

export const metadata: Metadata = { title: "初始化" };

export default function SetupPage() {
  return (
    <main className="flex flex-1 items-center justify-center bg-muted/40 p-4 md:p-6">
      <div className="flex w-full max-w-md flex-col gap-6">
        <div className="flex flex-col gap-1 text-center">
          <h1 className="font-heading text-2xl font-semibold">Intern</h1>
          <p className="text-sm text-muted-foreground">
            数据保护控制台 · 首次初始化
          </p>
        </div>
        <Suspense>
          <SetupForm />
        </Suspense>
      </div>
    </main>
  );
}
