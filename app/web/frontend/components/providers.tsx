"use client";

import { QueryClientProvider } from "@tanstack/react-query";
import { usePathname, useRouter } from "next/navigation";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { Toaster } from "@/components/ui/toast";
import { TooltipProvider } from "@/components/ui/tooltip";
import "@/lib/api/client";
import { currentSessionQueryKey } from "@/lib/api/generated/@tanstack/react-query.gen";
import { loginPath, safeNextPath } from "@/lib/navigation";
import { forgetPrincipal } from "@/lib/principal";
import { createQueryClient } from "@/lib/query-client";

export function Providers({ children }: { children: ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const redirecting = useRef(false);

  // One redirect per expiry: every query that fails with 401 at the same time
  // reaches the handler, and only the first one still knows whether a session
  // had been loaded before the cache is cleared.
  useEffect(() => {
    if (pathname === "/login" || pathname === "/setup") {
      redirecting.current = false;
    }
  }, [pathname]);

  // A page restored from the back-forward cache still shows the previous
  // principal's data (Cache-Control: no-cache does not prevent that), for
  // example after signing out and pressing Back. Reload it so the session is
  // read again and an ended session lands on the login page.
  useEffect(() => {
    const onPageShow = (event: PageTransitionEvent) => {
      if (event.persisted) {
        window.location.reload();
      }
    };
    window.addEventListener("pageshow", onPageShow);
    return () => window.removeEventListener("pageshow", onPageShow);
  }, []);

  const [queryClient] = useState(() => {
    const client = createQueryClient(() => {
      const publicPath =
        window.location.pathname === "/login" ||
        window.location.pathname === "/setup";
      if (redirecting.current || publicPath) {
        return;
      }
      redirecting.current = true;
      const hadSession =
        client.getQueryData(currentSessionQueryKey()) !== undefined;
      const next = safeNextPath(
        window.location.pathname + window.location.search,
      );
      forgetPrincipal(client);
      router.replace(loginPath(next, hadSession ? "expired" : undefined));
    });
    return client;
  });

  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <Toaster>{children}</Toaster>
      </TooltipProvider>
    </QueryClientProvider>
  );
}
