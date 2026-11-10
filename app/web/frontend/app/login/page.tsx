import type { Metadata } from "next";
import { Suspense } from "react";
import { LoginForm } from "./login-form";

export const metadata: Metadata = { title: "登录" };

export default function LoginPage() {
  return (
    <main className="flex flex-1 items-center justify-center bg-muted/40 p-4 md:p-6">
      <div className="flex w-full max-w-sm flex-col gap-6">
        <div className="flex flex-col gap-1 text-center">
          <h1 className="font-heading text-2xl font-semibold">Intern</h1>
          <p className="text-sm text-muted-foreground">数据保护控制台</p>
        </div>
        {/* useSearchParams in a static export needs a Suspense boundary. */}
        <Suspense>
          <LoginForm />
        </Suspense>
      </div>
    </main>
  );
}
