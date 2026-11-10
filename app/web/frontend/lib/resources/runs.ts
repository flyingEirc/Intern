import type {
  ReplicaStatus,
  RunErrorClass,
  RunKind,
  RunStatus,
  RunTriggerKind,
} from "@/lib/api/generated/types.gen";

/** Closed run statuses, in the order a filter should offer them. */
export const runStatuses = [
  "queued",
  "running",
  "completed",
  "failed",
  "canceled",
] as const satisfies readonly RunStatus[];

export const runStatusLabel: Record<RunStatus, string> = {
  queued: "排队中",
  running: "运行中",
  completed: "已完成",
  failed: "失败",
  canceled: "已取消",
};

export const runKindLabel: Record<RunKind, string> = {
  full: "全量",
  incremental: "增量",
};

export const runTriggerLabel: Record<RunTriggerKind, string> = {
  cron: "调度",
  manual: "手动",
  admission: "准入",
};

export const runErrorClassLabel: Record<RunErrorClass, string> = {
  unavailable: "部署不可用",
  preflight: "预检失败",
  requires_full: "需要全量",
  canceled: "已取消",
  interrupted: "被中断",
  backup_failed: "备份失败",
  recovery_failed: "恢复失败",
};

/** Closed replica statuses, in the order a filter should offer them. */
export const replicaStatuses = [
  "pending",
  "uploading",
  "uploaded",
  "verified",
  "failed",
  "exhausted",
] as const satisfies readonly ReplicaStatus[];

export const replicaStatusLabel: Record<ReplicaStatus, string> = {
  pending: "等待复制",
  uploading: "复制中",
  uploaded: "已上传",
  verified: "已校验",
  failed: "失败",
  exhausted: "已耗尽",
};

/** Statuses a run leaves only through a terminal write. */
export function isTerminalRun(status: RunStatus): boolean {
  return status === "completed" || status === "failed" || status === "canceled";
}

const backupKindLabel: Record<string, string> = {
  "physical-full": "物理全量",
  "physical-incremental": "物理增量",
};

export function backupKindText(kind: string): string {
  return backupKindLabel[kind] ?? kind;
}

const backupStatusLabel: Record<string, string> = {
  running: "进行中",
  completed: "已完成",
  failed: "失败",
};

export function backupStatusText(status: string): string {
  return backupStatusLabel[status] ?? status;
}

export const runEventLabel: Record<string, string> = {
  queued: "排队",
  claimed: "开始执行",
  preflight_ok: "环境检查通过",
  preflight_failed: "环境检查未通过",
  started: "备份开始",
  completed: "备份完成",
  failed: "运行失败",
  canceled: "已取消",
  interrupted: "被中断",
  replication_enqueued: "已加入异地复制",
  replication_step: "复制进度",
  truncated: "日志已截断",
};

export const runLevelLabel: Record<string, string> = {
  debug: "调试",
  info: "信息",
  warn: "警告",
  error: "错误",
};

export function runEventText(event: string): string {
  return runEventLabel[event] ?? "运行事件";
}

export function runFailureReason(
  errorClass: string | null,
  errorCode: string | null,
): string | null {
  if (errorClass === null && errorCode === null) {
    return "本次运行失败，请查看技术诊断或联系支持。";
  }
  switch (errorClass) {
    case "unavailable":
      return "部署缺少备份所需的配置，本次备份没有开始。";
    case "preflight":
      return "备份环境检查未通过，本次备份没有开始。";
    case "requires_full":
      return "需要先完成一次全量备份，之后才能继续增量备份。";
    case "canceled":
      return "本次运行已取消。";
    case "interrupted":
      return "本次运行被中断，可能没有生成完整备份。";
    case "backup_failed":
      return "备份过程中失败，没有生成可用的备份。";
    case "recovery_failed":
      return "操作过程中失败，请查看技术诊断或联系支持。";
    default:
      return "本次运行失败，请查看技术诊断或联系支持。";
  }
}
