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
    if (pathname === "/login") {
      redirecting.current = false;
    }
  }, [pathname]);

  const [queryClient] = useState(() => {
    const client = createQueryClient(() => {
      if (redirecting.current || window.location.pathname === "/login") {
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
