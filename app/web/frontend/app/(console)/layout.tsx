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
      <div className="flex flex-1 flex-col gap-4 p-4 md:p-6 lg:p-8">
        <Skeleton className="h-9 w-56" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-48 w-full" />
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
      <a
        href="#console-content"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:rounded-md focus:bg-primary focus:px-3 focus:py-2 focus:text-primary-foreground"
      >
        跳到主要内容
      </a>
      <AppSidebar
        user={session.data.user}
        permissions={session.data.permissions}
      />
      <SidebarInset className="min-w-0">
        <header className="sticky top-0 flex h-16 shrink-0 items-center gap-2 border-b bg-background/95 px-4 backdrop-blur md:px-6">
          <SidebarTrigger className="-ml-1" />
          <Separator
            orientation="vertical"
            className="mr-2 data-vertical:h-4 data-vertical:self-auto"
          />
          <div className="flex min-w-0 flex-col">
            <p className="text-[0.7rem] tracking-wide text-muted-foreground uppercase">
              数据保护控制台
            </p>
            <p className="truncate font-heading text-sm font-medium">
              {navTitle(pathname)}
            </p>
          </div>
        </header>
        <main
          id="console-content"
          className="mx-auto flex w-full min-w-0 max-w-[1600px] flex-1 flex-col gap-5 p-4 md:gap-6 md:p-6 lg:p-8"
        >
          {children}
        </main>
      </SidebarInset>
    </SidebarProvider>
  );
}
