import { ApiError } from "@/lib/api/errors";
import type {
  RestoreErrorClass,
  RestorePhase,
  RestorePlanLink,
  RestoreSideEffects,
  RestoreStatus,
  RestoreVerifyStatus,
} from "@/lib/api/generated/types.gen";
import { describeError } from "@/lib/api/messages";

/**
 * The restore engine's verify_sql bounds (`internal/restore`
 * `MaxVerifySQLStatements` and `MaxVerifySQLBytes`), which the OpenAPI
 * contract mirrors exactly. The engine remains the authority: these checks
 * only stop a request the engine would refuse.
 */
export const maxVerifyStatements = 16;
export const maxVerifyStatementBytes = 4096;

/** Closed restore statuses, in the order a filter should offer them. */
export const restoreStatuses = [
  "queued",
  "running",
  "completed",
  "failed",
  "canceled",
] as const satisfies readonly RestoreStatus[];

export const restoreStatusLabel: Record<RestoreStatus, string> = {
  queued: "排队中",
  running: "执行中",
  completed: "已完成",
  failed: "失败",
  canceled: "已取消",
};

export const restorePhaseLabel: Record<RestorePhase, string> = {
  fetching: "获取备份",
  authenticating: "认证载荷",
  extracting: "解包",
  preparing: "prepare",
  exporting: "发布导出",
  rehearsing: "演练",
};

/** The order the engine records phases in, for the detail log. */
export const restorePhaseOrder = [
  "fetching",
  "authenticating",
  "extracting",
  "preparing",
  "exporting",
  "rehearsing",
] as const satisfies readonly RestorePhase[];

export const restoreErrorClassLabel: Record<RestoreErrorClass, string> = {
  unavailable: "部署不可用",
  refused: "计划被拒绝",
  restore_failed: "恢复失败",
  canceled: "已取消",
  interrupted: "被中断",
};

export const restoreVerifyStatusLabel: Record<RestoreVerifyStatus, string> = {
  not_requested: "未请求演练",
  pending: "演练尚未完成",
  passed: "演练通过",
  failed: "演练失败",
  not_run: "演练未执行",
};

export const linkSourceLabel: Record<RestorePlanLink["source"], string> = {
  local: "本地仓库",
  ftp: "FTP 副本",
};

/**
 * Advisory plan risk codes the engine emits. An unknown code still renders,
 * with its raw value, so a newer engine does not disappear from the plan.
 */
export const riskExplanation: Record<string, string> = {
  no_auto_replace_production:
    "恢复只会把已 prepare 的数据目录发布到导出路径，从不自动替换生产实例的 datadir。",
  server_uuid_duplicate:
    "导出保留源实例的 server-uuid（auto.cnf）。替换生产实例前必须先停源库，并核对该 uuid：相同的 server_uuid 不能与源实例同时接入复制拓扑。",
  pitr_unavailable:
    "这是检查点恢复，不是任意时间点恢复。binlog 归档与完整物理 PITR 属于路线图 N1，尚未实现。",
  rehearsal_unavailable:
    "当前部署没有配置 mysqld，无法在隔离的演练实例上执行校验。恢复本身仍可执行。",
};

/**
 * Machine codes a `restore_refused` error carries in `error.detail.code`.
 * The backend message is shown alongside these explanations.
 */
export const refusalExplanation: Record<string, string> = {
  backup_not_found: "目录中没有这份备份。",
  backup_not_completed: "这份备份尚未完成，不能作为恢复点。",
  chain_broken: "备份链不完整：缺少从全量到该点的某一环。",
  lsn_discontinuous: "链上的 LSN 不连续，无法确定恢复范围。",
  source_unavailable: "没有可用来源：本地载荷不在，也没有已校验的 FTP 副本。",
  tool_version_mismatch: "备份时的工具版本与当前部署不一致。",
  matrix_unsupported: "源实例或工具版本不在已支持的版本矩阵内。",
  key_unavailable: "解密这份备份所需的密钥不可用。",
  insufficient_space: "工作目录或导出目录的可用空间不足以展开这条链。",
  plan_changed: "确认后目录发生了变化，原计划不再有效。",
  unconfigured: "当前部署没有配齐恢复所需的输入。",
};

/** Statuses a restore leaves only through a terminal write. */
export function isTerminalRestore(status: RestoreStatus): boolean {
  return status === "completed" || status === "failed" || status === "canceled";
}

/** The three terminal side effects, each stated as what did or did not happen. */
export function sideEffectStatements(
  effects: RestoreSideEffects,
): [string, string, string] {
  return [
    effects.work_dir_removed ? "工作目录已删除" : "工作目录未删除",
    effects.pending_export_removed ? "待发布导出已删除" : "待发布导出未删除",
    effects.export_published ? "导出已发布" : "导出未发布",
  ];
}

export function riskText(code: string): string {
  return riskExplanation[code] ?? "未知风险码，请按其原始码核对。";
}

export function refusalText(code: string | undefined): string {
  if (code === undefined || code === "") {
    return "服务拒绝了这次恢复，但没有给出机器码。";
  }
  return refusalExplanation[code] ?? "未知的拒绝原因，请按机器码核对。";
}

/** First and last twelve characters of a 64-character digest, for display. */
export function shortenDigest(digest: string): string {
  if (digest.length <= 24) {
    return digest;
  }
  return `${digest.slice(0, 12)}…${digest.slice(-12)}`;
}

export type VerifySqlError =
  | { ok: true; statements: string[] }
  | { ok: false; message: string };

/**
 * Splits the editor text into statements and applies the engine's rules: at
 * most 16 statements, each 1 to 4096 bytes, no NUL, no inner `;` (one trailing
 * `;` is kept), and each a single read-only SELECT. Blank lines are skipped.
 * An empty editor is zero statements, which the contract accepts.
 */
export function parseVerifySql(text: string): VerifySqlError {
  const statements = text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
  if (statements.length > maxVerifyStatements) {
    return {
      ok: false,
      message: `校验语句最多 ${maxVerifyStatements} 条，当前 ${statements.length} 条。请删减后再执行。`,
    };
  }
  for (const [index, statement] of statements.entries()) {
    const problem = verifyStatementProblem(statement);
    if (problem !== null) {
      return { ok: false, message: `第 ${index + 1} 条：${problem}` };
    }
  }
  return { ok: true, statements };
}

function verifyStatementProblem(statement: string): string | null {
  const bytes = new TextEncoder().encode(statement).length;
  if (bytes > maxVerifyStatementBytes || statement.includes("\0")) {
    return `每条语句必须为 1 到 ${maxVerifyStatementBytes} 字节，且不能包含 NUL。`;
  }
  const trimmed = statement.trimEnd();
  const body = trimmed.endsWith(";")
    ? trimmed.slice(0, -1).trim()
    : statement.trim();
  if (body === "" || body.includes(";")) {
    return "每条只能是一条语句；允许一个结尾分号，不能再包含其他分号。";
  }
  const lower = body.replace(/^[ \t\r\n(]+/, "").toLowerCase();
  const boundary = lower.charAt(6);
  const select =
    lower.startsWith("select") &&
    (lower.length === 6 ||
      boundary === " " ||
      boundary === "\t" ||
      boundary === "\n" ||
      boundary === "\r" ||
      boundary === "(");
  if (!select) {
    return "每条必须是只读的 SELECT 语句（SELECT 后接空白、换行或左括号）。";
  }
  return null;
}

const recordedPhases = new Set<string>(restorePhaseOrder);

/** Phases named by the restore log, in the order the engine first recorded them. */
export function phasesRecorded(
  logs: ReadonlyArray<{
    seq: number;
    message: string;
    data: { [key: string]: unknown } | null;
  }>,
): RestorePhase[] {
  const seen: RestorePhase[] = [];
  const ordered = [...logs].sort((left, right) => left.seq - right.seq);
  for (const log of ordered) {
    const fromData = log.data?.phase;
    const fromMessage = /^restore phase ([a-z]+)$/.exec(log.message)?.[1];
    const phase = typeof fromData === "string" ? fromData : fromMessage;
    if (
      phase !== undefined &&
      recordedPhases.has(phase) &&
      !seen.includes(phase as RestorePhase)
    ) {
      seen.push(phase as RestorePhase);
    }
  }
  return seen;
}

/** A 422 validation failure pinned to the rehearsal controls when the message names them. */
export function restoreValidationFields(error: unknown): {
  verifySql?: string;
  rehearse?: string;
} {
  if (
    !(error instanceof ApiError) ||
    error.status !== 422 ||
    error.code !== "validation_failed"
  ) {
    return {};
  }
  const message = error.message.toLowerCase();
  if (message.includes("verify")) {
    return { verifySql: error.message };
  }
  if (message.includes("rehears")) {
    return { rehearse: error.message };
  }
  return {};
}

export function isRestoreRefused(error: unknown): error is ApiError {
  return error instanceof ApiError && error.code === "restore_refused";
}

export function isPlanChanged(error: unknown): error is ApiError {
  return (
    error instanceof ApiError &&
    error.status === 409 &&
    error.code === "plan_changed"
  );
}

export function isConfirmationFailed(error: unknown): error is ApiError {
  return (
    error instanceof ApiError &&
    error.status === 403 &&
    error.code === "confirmation_failed"
  );
}

export function isRateLimited(error: unknown): error is ApiError {
  return error instanceof ApiError && error.status === 429;
}

/**
 * Page-level Chinese text for a failed restore request. `restore_refused`
 * names the machine code and its explanation plus the backend message;
 * `plan_changed` and `confirmation_failed` have their own panels and are not
 * described here.
 */
export function describeRestoreError(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return describeError(error);
  }
  if (error.code === "restore_refused") {
    const code = error.detailCode ?? "未提供";
    return `恢复被拒绝（${code}）：${refusalText(error.detailCode)}服务返回：${error.message}`;
  }
  if (error.code === "rate_limited") {
    const wait =
      error.retryAfter === undefined
        ? "请稍后再试"
        : `请 ${error.retryAfter} 秒后再试`;
    return `密码确认尝试过于频繁，${wait}。改密与恢复确认共用同一限速，每个用户每分钟共 5 次。`;
  }
  return describeError(error);
}

/**
 * The exact command operations.md documents for starting a fresh MySQL
 * container on a published export. The host directory is the operator's
 * `INTERN_EXPORT_HOST_DIR`; the image is the deployment's pinned MySQL image.
 */
export function exportStartCommand(restoreId: string): string {
  return [
    "docker run --rm --platform linux/amd64 -u 999:999 \\",
    '  -v "$PWD/exports:/exports" \\',
    '  "$INTERN_MYSQL_IMAGE" \\',
    `  --datadir=/exports/${restoreId}/data --socket=/tmp/mysql.sock --pid-file=/tmp/mysqld.pid \\`,
    "  --port=3306 --skip-log-bin --innodb-buffer-pool-size=64M",
  ].join("\n");
}
