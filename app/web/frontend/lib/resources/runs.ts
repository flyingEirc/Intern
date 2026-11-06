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
