import { toApiError } from "./errors";
import { client } from "./generated/client.gen";

// Installed once per module instance; `exists` keeps a hot reload from stacking
// a second copy.
if (!client.interceptors.error.exists(toApiError)) {
  client.interceptors.error.use(toApiError);
}

export { client };
