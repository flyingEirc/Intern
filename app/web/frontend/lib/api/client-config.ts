import type { CreateClientConfig } from "./generated/client.gen";

// The UI is always served from the same origin as /api/v1 (embedded in
// intern-web, or proxied by `next dev`), so requests are relative and carry the
// session cookie only to that origin.
export const createClientConfig: CreateClientConfig = (config) => ({
  ...config,
  baseUrl: "",
  credentials: "same-origin",
});
