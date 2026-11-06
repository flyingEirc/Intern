"use client";

import { CircleAlertIcon } from "lucide-react";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { AppSidebar } from "@/components/app-sidebar";
import { navTitle } from "@/components/nav";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  SidebarInset,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
import { Skeleton } from "@/components/ui/skeleton";
import { useSession } from "@/hooks/use-session";
import { isUnauthorized } from "@/lib/api/errors";
import { describeError } from "@/lib/api/messages";

function ConsoleSkeleton() {
  return (
    <div className="flex flex-1" aria-busy="true">
      <span className="sr-only">正在加载会话</span>
      <Skeleton className="hidden h-svh w-64 rounded-none md:block" />
      <div className="flex flex-1 flex-col gap-4 p-6">
        <Skeleton className="h-8 w-48" />
        <Skeleton className="h-32 w-full" />
      </div>
    </div>
  );
}

export default function ConsoleLayout({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const session = useSession();

  if (session.isPending) {
    return <ConsoleSkeleton />;
  }
  if (session.isError) {
    // 401 is already being redirected to /login by the query cache.
    if (isUnauthorized(session.error)) {
      return <ConsoleSkeleton />;
    }
    return (
      <main className="flex flex-1 items-center justify-center p-6">
        <Alert variant="destructive" className="max-w-md">
          <CircleAlertIcon />
          <AlertTitle>无法读取会话</AlertTitle>
          <AlertDescription className="flex flex-col items-start gap-3">
            {describeError(session.error)}
            <Button
              variant="outline"
              size="sm"
              onClick={() => session.refetch()}
            >
              重试
            </Button>
          </AlertDescription>
        </Alert>
      </main>
    );
  }

  return (
    <SidebarProvider>
      <AppSidebar user={session.data.user} />
      <SidebarInset>
        <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
          <SidebarTrigger className="-ml-1" />
          <Separator
            orientation="vertical"
            className="mr-2 data-vertical:h-4 data-vertical:self-auto"
          />
          <h1 className="font-heading text-base font-medium">
            {navTitle(pathname)}
          </h1>
        </header>
        <div className="flex flex-1 flex-col gap-6 p-6">{children}</div>
      </SidebarInset>
    </SidebarProvider>
  );
}
