import type { QueryClient } from "@tanstack/react-query";
import { toast } from "@/components/ui/toast";

/**
 * Drops everything the previous principal could see: cached API data and
 * pending notifications. Called on sign-in, sign-out and session expiry.
 */
export function forgetPrincipal(queryClient: QueryClient): void {
  queryClient.clear();
  toast.close();
}
