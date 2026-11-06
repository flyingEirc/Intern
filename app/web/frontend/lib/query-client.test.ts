import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api/errors";
import { createQueryClient } from "./query-client";

const unauthorized = () =>
  new ApiError({
    status: 401,
    code: "unauthorized",
    message: "Authentication required",
  });

describe("createQueryClient session expiry", () => {
  it("redirects once a protected query answers 401, without retrying", async () => {
    const onUnauthorized = vi.fn();
    const client = createQueryClient(onUnauthorized);
    const queryFn = vi.fn().mockRejectedValue(unauthorized());
    await client.fetchQuery({ queryKey: ["runs"], queryFn }).catch(() => {});
    expect(queryFn).toHaveBeenCalledTimes(1);
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it("leaves 401 to requests that expect it", async () => {
    const onUnauthorized = vi.fn();
    const client = createQueryClient(onUnauthorized);
    await client
      .fetchQuery({
        queryKey: ["session"],
        queryFn: () => Promise.reject(unauthorized()),
        meta: { handlesUnauthorized: true },
      })
      .catch(() => {});
    await client
      .getMutationCache()
      .build(client, {
        mutationFn: () => Promise.reject(unauthorized()),
        meta: { handlesUnauthorized: true },
      })
      .execute(undefined)
      .catch(() => {});
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("redirects when a mutation answers 401", async () => {
    const onUnauthorized = vi.fn();
    const client = createQueryClient(onUnauthorized);
    await client
      .getMutationCache()
      .build(client, { mutationFn: () => Promise.reject(unauthorized()) })
      .execute(undefined)
      .catch(() => {});
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it("does not treat 403 as an expired session", async () => {
    const onUnauthorized = vi.fn();
    const client = createQueryClient(onUnauthorized);
    await client
      .fetchQuery({
        queryKey: ["users"],
        queryFn: () =>
          Promise.reject(
            new ApiError({
              status: 403,
              code: "forbidden",
              message: "Forbidden",
            }),
          ),
      })
      .catch(() => {});
    expect(onUnauthorized).not.toHaveBeenCalled();
  });
});
