import path from "node:path";
import type { NextConfig } from "next";
import { PHASE_DEVELOPMENT_SERVER } from "next/constants";

// The UI ships as a static export embedded in intern-web and served from the
// same origin as /api/v1: the backend rejects cross-origin unsafe requests and
// its session cookie is Secure + SameSite=Lax. A static export cannot carry
// rewrites, so only `next dev` proxies the API to a local backend
// (INTERN_DEV_BACKEND, default http://127.0.0.1:8080) to stay same-origin.
// create-next-app enables cacheComponents and partialPrefetching by default;
// both are Partial Prerendering, which `next build` rejects in export mode
// ("PPR cannot be enabled in export mode"), so they are off.
export default function config(phase: string): NextConfig {
  const base: NextConfig = {
    // `_next/static/<buildId>/` is served as immutable, so the id must change
    // with the source. Builds pass the commit through INTERN_BUILD_ID (mise,
    // CI and the image do); without it Next generates a random id.
    generateBuildId: async () => process.env.INTERN_BUILD_ID ?? null,
    images: { unoptimized: true },
    turbopack: {
      // Pin the workspace root: a lockfile in a parent directory must not
      // change module resolution.
      root: path.join(__dirname),
      rules: {
        "*.css": {
          loaders: ["@tailwindcss/turbopack"],
          as: "*.css",
        },
      },
    },
  };
  if (phase !== PHASE_DEVELOPMENT_SERVER) {
    return { ...base, output: "export" };
  }
  const backend = process.env.INTERN_DEV_BACKEND ?? "http://127.0.0.1:8080";
  return {
    ...base,
    async rewrites() {
      return [
        { source: "/api/:path*", destination: `${backend}/api/:path*` },
        { source: "/healthz", destination: `${backend}/healthz` },
        { source: "/readyz", destination: `${backend}/readyz` },
      ];
    },
  };
}
