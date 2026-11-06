import { ApiError, toApiError } from "./errors";
import { client } from "./generated/client.gen";

/**
 * The generated client answers any 204, or any 2xx with `Content-Length: 0`,
 * with `{}` and never runs the operation's Zod validator on it. Run it here on
 * the absent body instead: an operation the contract declares as 204 has a
 * `z.void()` validator and accepts, while one that promises a JSON body (the
 * session, readiness, ...) rejects the empty success as a contract violation.
 */
async function validateEmptySuccess(
  response: Response,
  _request: Request,
  options: { responseValidator?: (data: unknown) => Promise<unknown> },
): Promise<Response> {
  const empty =
    response.status === 204 || response.headers.get("Content-Length") === "0";
  if (!response.ok || !empty || !options.responseValidator) {
    return response;
  }
  try {
    await options.responseValidator(undefined);
  } catch {
    throw new ApiError({
      status: response.status,
      code: "contract",
      message: "Response does not match the API contract",
      requestId: response.headers.get("X-Request-ID") ?? undefined,
    });
  }
  return response;
}

// Installed once per module instance; `exists` keeps a hot reload from stacking
// a second copy.
if (!client.interceptors.response.exists(validateEmptySuccess)) {
  client.interceptors.response.use(validateEmptySuccess);
}
if (!client.interceptors.error.exists(toApiError)) {
  client.interceptors.error.use(toApiError);
}

export { client };
