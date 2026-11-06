import { ApiError } from "./errors";

function withRequestId(message: string, error: ApiError): string {
  return error.requestId ? `${message}（请求 ID ${error.requestId}）` : message;
}

function retryHint(error: ApiError): string {
  return error.retryAfter === undefined
    ? "请稍后重试"
    : `请 ${error.retryAfter} 秒后重试`;
}

/** Operator-facing Chinese text for any failed request. */
export function describeError(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return "发生未知错误";
  }
  switch (error.code) {
    case "network":
      return "无法连接 Intern 服务，请检查网络或服务状态";
    case "contract":
      return withRequestId("服务返回了不符合接口契约的响应", error);
    case "rate_limited":
      return `尝试过于频繁，${retryHint(error)}`;
    case "unavailable":
      return withRequestId(`服务暂时不可用，${retryHint(error)}`, error);
    case "cross_origin_rejected":
      return "请求被跨源保护拒绝，请从 Intern 服务地址直接访问";
    case "forbidden":
      return withRequestId("当前账号没有执行此操作的权限", error);
    default:
      // The backend's message names the failed rule (for example which field
      // failed validation); it is English and shown as reported.
      return withRequestId(
        `请求失败（HTTP ${error.status}，${error.code}）：${error.message}`,
        error,
      );
  }
}
