import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { client } from "./client";
import { ApiError } from "./errors";
import { currentSession, login } from "./generated/sdk.gen";

const user = {
  id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a3b",
  username: "admin",
  role: "admin",
  disabled: false,
  created_at: "2026-11-06T08:00:00Z",
  updated_at: "2026-11-06T08:00:00Z",
};

function json(status: number, body: unknown, headers: HeadersInit = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

const fetchMock = vi.fn<typeof fetch>();

beforeEach(() => {
  client.setConfig({ baseUrl: "http://intern.test", fetch: fetchMock });
});

afterEach(() => {
  fetchMock.mockReset();
});

describe("generated SDK with the contract error interceptor", () => {
  it("resolves a response that matches the contract", async () => {
    fetchMock.mockResolvedValue(
      json(200, { user, permissions: ["session:self", "users:manage"] }),
    );
    const { data } = await currentSession({ throwOnError: true });
    expect(data.user.username).toBe("admin");
    const request = fetchMock.mock.calls[0][0] as Request;
    expect(request.url).toBe("http://intern.test/api/v1/auth/session");
  });

  it("rejects a 2xx body that violates the contract", async () => {
    // permissions contains a value the Permission enum does not define.
    fetchMock.mockResolvedValue(
      json(200, { user, permissions: ["root"] }, { "X-Request-ID": "req-1" }),
    );
    const failure = await currentSession({ throwOnError: true }).catch(
      (error: unknown) => error,
    );
    expect(failure).toBeInstanceOf(ApiError);
    expect(failure).toMatchObject({
      status: 200,
      code: "contract",
      requestId: "req-1",
    });
  });

  it("maps the contract error envelope, including Retry-After", async () => {
    fetchMock.mockResolvedValue(
      json(
        429,
        {
          error: {
            code: "rate_limited",
            message: "Too many login attempts",
            request_id: "req-2",
          },
        },
        { "Retry-After": "17" },
      ),
    );
    const failure = await login({
      body: { username: "admin", password: "x" },
      throwOnError: true,
    }).catch((error: unknown) => error);
    expect(failure).toMatchObject({
      status: 429,
      code: "rate_limited",
      message: "Too many login attempts",
      requestId: "req-2",
      retryAfter: 17,
    });
  });

  it("treats an error body without the envelope as a contract violation", async () => {
    fetchMock.mockResolvedValue(
      new Response("<html>bad gateway</html>", { status: 502 }),
    );
    const failure = await currentSession({ throwOnError: true }).catch(
      (error: unknown) => error,
    );
    expect(failure).toMatchObject({ status: 502, code: "contract" });
  });

  it("reports a failed fetch as a network error", async () => {
    fetchMock.mockRejectedValue(new TypeError("Failed to fetch"));
    const failure = await currentSession({ throwOnError: true }).catch(
      (error: unknown) => error,
    );
    expect(failure).toMatchObject({ status: 0, code: "network" });
  });
});
