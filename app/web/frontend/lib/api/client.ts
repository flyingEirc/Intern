import { ApiError, toApiError } from "./errors";
import { client } from "./generated/client.gen";

/**
 * The generated client treats any 2xx with `Content-Length: 0` like a 204 and
 * returns `{}` without running the Zod validator. In the contract only 204
 * answers have no body (the SSE stream uses its own client), so an empty body
 * on any other 2xx is a contract violation and must fail like one.
 */
async function rejectEmptySuccess(response: Response): Promise<Response> {
  if (
    response.ok &&
    response.status !== 204 &&
    response.headers.get("Content-Length") === "0"
  ) {
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
if (!client.interceptors.response.exists(rejectEmptySuccess)) {
  client.interceptors.response.use(rejectEmptySuccess);
}
if (!client.interceptors.error.exists(toApiError)) {
  client.interceptors.error.use(toApiError);
}

export { client };
