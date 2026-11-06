import { defineConfig } from "@hey-api/openapi-ts";

// The backend's handwritten OpenAPI document is the only contract. The
// generated client is committed; `bun run generate:check` regenerates it and
// fails when the committed copy has drifted from app/web/api/openapi.yaml.
export default defineConfig({
  input: "../api/openapi.yaml",
  output: {
    path: "lib/api/generated",
  },
  plugins: [
    {
      name: "@hey-api/client-fetch",
      runtimeConfigPath: "./lib/api/client-config",
    },
    "zod",
    {
      name: "@hey-api/sdk",
      validator: { response: "zod" },
    },
    {
      name: "@tanstack/react-query",
      queryOptions: true,
      mutationOptions: true,
    },
  ],
});
