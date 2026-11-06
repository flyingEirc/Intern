const probeOrigin = "http://intern.invalid";

/**
 * Accepts a post-login destination only when it stays on this origin. The
 * value is resolved by the same URL parser the browser navigates with, so a
 * scheme-relative `//host`, a backslash, or embedded tabs and newlines that a
 * browser would normalise into another origin are all rejected. `/login`
 * itself falls back to `/` so a signed-in visit cannot loop.
 */
export function safeNextPath(raw: string | null | undefined): string {
  if (!raw?.startsWith("/")) {
    return "/";
  }
  let url: URL;
  try {
    url = new URL(raw, probeOrigin);
  } catch {
    return "/";
  }
  if (url.origin !== probeOrigin || url.pathname === "/login") {
    return "/";
  }
  return `${url.pathname}${url.search}${url.hash}`;
}

export type LoginReason = "expired" | "password-changed" | "signed-out";

export function loginPath(next: string, reason?: LoginReason): string {
  const params = new URLSearchParams();
  if (next !== "/") {
    params.set("next", next);
  }
  if (reason) {
    params.set("reason", reason);
  }
  const query = params.toString();
  return query ? `/login?${query}` : "/login";
}
