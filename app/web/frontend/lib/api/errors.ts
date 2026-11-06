import type { z } from "zod";
import type { PreflightResult } from "./generated/types.gen";
import { zErrorResponse } from "./generated/zod.gen";

type ContractErrorCode = z.infer<typeof zErrorResponse>["error"]["code"];

/**
 * `network`: no HTTP response (offline, connection refused, CORS).
 * `contract`: a response the OpenAPI contract does not describe, either a
 * success body that failed Zod validation or an error body without the
 * contract's error envelope.
 */
export type ApiErrorCode = ContractErrorCode | "network" | "contract";

/** Every failed SDK call rejects with this, never with a raw body. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: ApiErrorCode;
  readonly requestId: string | undefined;
  /** Whole seconds from Retry-After on 429 and 503, when present. */
  readonly retryAfter: number | undefined;
  /**
   * The structured preflight outcome of a 409 `preflight_failed`. The error
   * envelope carries it; the message alone cannot describe which check failed.
   */
  readonly preflight: PreflightResult | undefined;

  constructor(init: {
    status: number;
    code: ApiErrorCode;
    message: string;
    requestId?: string;
    retryAfter?: number;
    preflight?: PreflightResult;
  }) {
    super(init.message);
    this.name = "ApiError";
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId;
    this.retryAfter = init.retryAfter;
    this.preflight = init.preflight;
  }
}

function retryAfterSeconds(response: Response): number | undefined {
  const raw = response.headers.get("Retry-After");
  if (raw === null || !/^\d+$/.test(raw)) {
    return undefined;
  }
  return Number(raw);
}

/**
 * Error interceptor for the generated fetch client. It receives whatever the
 * client threw: the parsed error body for a non-2xx response, a Zod error for a
 * 2xx body that violates the contract, or a fetch failure with no response.
 */
export function toApiError(
  error: unknown,
  response: Response | undefined,
): ApiError {
  if (error instanceof ApiError) {
    return error;
  }
  if (response === undefined) {
    return new ApiError({
      status: 0,
      code: "network",
      message: error instanceof Error ? error.message : "Network error",
    });
  }
  const requestId = response.headers.get("X-Request-ID") ?? undefined;
  if (response.ok) {
    return new ApiError({
      status: response.status,
      code: "contract",
      message: "Response does not match the API contract",
      requestId,
    });
  }
  const parsed = zErrorResponse.safeParse(error);
  if (!parsed.success) {
    return new ApiError({
      status: response.status,
      code: "contract",
      message: `HTTP ${response.status}`,
      requestId,
      retryAfter: retryAfterSeconds(response),
    });
  }
  return new ApiError({
    status: response.status,
    code: parsed.data.error.code,
    message: parsed.data.error.message,
    requestId: parsed.data.error.request_id,
    retryAfter: retryAfterSeconds(response),
    preflight: parsed.data.error.preflight,
  });
}

export function isUnauthorized(error: unknown): boolean {
  return error instanceof ApiError && error.status === 401;
}
