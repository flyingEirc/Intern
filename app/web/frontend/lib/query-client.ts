import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import { ApiError, isUnauthorized } from "./api/errors";

declare module "@tanstack/react-query" {
  interface Register {
    queryMeta: AuthMeta;
    mutationMeta: AuthMeta;
  }
}

/**
 * `handlesUnauthorized`: this request treats 401 as an expected answer (the
 * login form, the login page's "already signed in" probe, logout), so the
 * global session-expiry redirect must not fire for it.
 */
export type AuthMeta = { handlesUnauthorized?: boolean };

function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) {
    return false;
  }
  if (error instanceof ApiError && error.code === "contract") {
    return false;
  }
  return failureCount < 2;
}

export function createQueryClient(onUnauthorized: () => void): QueryClient {
  return new QueryClient({
    queryCache: new QueryCache({
      onError: (error, query) => {
        if (isUnauthorized(error) && !query.meta?.handlesUnauthorized) {
          onUnauthorized();
        }
      },
    }),
    mutationCache: new MutationCache({
      onError: (error, _variables, _context, mutation) => {
        if (isUnauthorized(error) && !mutation.meta?.handlesUnauthorized) {
          onUnauthorized();
        }
      },
    }),
    defaultOptions: {
      queries: {
        retry: shouldRetry,
        refetchOnWindowFocus: true,
        staleTime: 15_000,
      },
      mutations: { retry: false },
    },
  });
}
