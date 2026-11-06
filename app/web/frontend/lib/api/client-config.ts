import type { CreateClientConfig } from "./generated/client.gen";

// The UI is always served from the same origin as /api/v1 (embedded in
// intern-web, or proxied by `next dev`), so requests are relative and carry the
// session cookie only to that origin.
//
// Every non-streaming operation of the contract answers application/json, so
// bodies are always parsed as JSON and run through the Zod validator. The
// default `auto` would hand a 2xx HTML page (a proxy's login page, say) to the
// UI as a string without validation. 204 stays an empty success; SSE uses its
// own client.
export const createClientConfig: CreateClientConfig = (config) => ({
  ...config,
  baseUrl: "",
  credentials: "same-origin",
  parseAs: "json",
});
