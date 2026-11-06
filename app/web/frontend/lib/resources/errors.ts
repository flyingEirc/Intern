import { ApiError } from "@/lib/api/errors";
import type { PreflightResult } from "@/lib/api/generated/types.gen";
import { describeError } from "@/lib/api/messages";

/**
 * Fixed Chinese explanation for `503 unavailable`. The deployment is missing
 * an input the operation needs (KEK, work root, XtraBackup, ...); the backend
 * deliberately does not name the absent path or key, so the page must not
 * invent one either. The backend message is appended as reported.
 */
export const unavailableExplanation =
  "当前部署缺少完成此操作所需的配置（例如 KEK、工作根目录或 XtraBackup），因此该操作当前不可用。请让运维补齐部署配置后重试。";

export function isUnavailable(error: unknown): boolean {
  return error instanceof ApiError && error.code === "unavailable";
}

/** The structured preflight outcome of a 409 `preflight_failed`, if present. */
export function preflightOf(error: unknown): PreflightResult | null {
  return error instanceof ApiError ? (error.preflight ?? null) : null;
}

export function isPreflightFailure(error: unknown): boolean {
  return preflightOf(error) !== null;
}

export function isConflict(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409;
}

export function isValidationFailure(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    (error.status === 400 || error.status === 422) &&
    error.code === "validation_failed"
  );
}

/** Page-level Chinese text for a failed resource request. */
export function describeResourceError(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return describeError(error);
  }
  switch (error.code) {
    case "unavailable":
      return `${unavailableExplanation}（服务返回：${error.message}）`;
    case "preflight_failed":
      return `预检未通过，未写入任何变更。${error.message}`;
    case "conflict":
      return `操作冲突：${error.message}`;
    case "validation_failed":
    case "bad_request":
      return `请求未通过校验：${error.message}`;
    default:
      return describeError(error);
  }
}

/**
 * Maps a backend validation message onto one form field when it names it. The
 * backend reports the failed field first (`root must be ...`, `ftp.port must
 * be ...`, `password is required for a source`), so the prefix is the mapping
 * key; anything else stays form-level.
 */
export function apiFieldErrors<T extends string>(
  error: unknown,
  fields: readonly T[],
): Partial<Record<T, string>> {
  if (!isValidationFailure(error) || !(error instanceof ApiError)) {
    return {};
  }
  const message = error.message;
  for (const field of fields) {
    if (message.startsWith(`${field} `) || message.startsWith(`${field}.`)) {
      return { [field]: message } as Partial<Record<T, string>>;
    }
  }
  const ftp = /^ftp\.([a-z_]+)\s/.exec(message);
  const nested = ftp?.[1] as T | undefined;
  if (nested !== undefined && fields.includes(nested)) {
    return { [nested]: message } as Partial<Record<T, string>>;
  }
  return {};
}
