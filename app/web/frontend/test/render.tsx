import { QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { vi } from "vitest";
import { Toaster } from "@/components/ui/toast";
import { client } from "@/lib/api/client";
import { createQueryClient } from "@/lib/query-client";

export const fetchMock = vi.fn<typeof fetch>();

export function json(
  status: number,
  body?: unknown,
  headers: HeadersInit = {},
) {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

export const user = {
  id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b",
  username: "admin",
  role: "admin" as const,
  disabled: false,
  created_at: "2026-11-06T08:00:00Z",
  updated_at: "2026-11-06T08:00:00Z",
};

export const unauthorizedBody = {
  error: {
    code: "unauthorized",
    message: "Authentication required",
    request_id: "req-401",
  },
};

/** Routes each request by "METHOD /path" to a queued response. */
export function routeFetch(routes: Record<string, Array<() => Response>>) {
  fetchMock.mockImplementation(async (input) => {
    const request = input as Request;
    const key = `${request.method} ${new URL(request.url).pathname}`;
    const next = routes[key]?.shift();
    if (!next) {
      throw new Error(`unexpected request ${key}`);
    }
    return next();
  });
}

export function renderWithClient(ui: ReactElement) {
  client.setConfig({ baseUrl: "http://intern.test", fetch: fetchMock });
  const onUnauthorized = vi.fn();
  const queryClient = createQueryClient(onUnauthorized);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Toaster>{ui}</Toaster>
    </QueryClientProvider>,
  );
  return { ...view, queryClient, onUnauthorized };
}
