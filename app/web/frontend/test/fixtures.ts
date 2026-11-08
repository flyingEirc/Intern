import type {
  Backup,
  Chain,
  Permission,
  Point,
  Policy,
  PolicySchedule,
  Protection,
  Replica,
  Run,
  RunLog,
} from "@/lib/api/generated/types.gen";
import { user } from "./render";

const createdAt = "2026-11-06T08:00:00Z";

export const policyId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a01";
export const sourceId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a02";
export const repositoryId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a03";
export const replicaRepositoryId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a04";
export const runId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a05";
export const backupId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a06";
export const chainId = "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a07";

export function sessionBody(permissions: Permission[]) {
  return { user, permissions };
}

const viewerPermissions: Permission[] = [
  "session:self",
  "resources:read",
  "runs:read",
];

const operatorPermissions: Permission[] = [
  ...viewerPermissions,
  "runs:trigger",
  "runs:cancel",
  "replicas:retry",
];

export const sessions = {
  viewer: () => sessionBody(viewerPermissions),
  operator: () => sessionBody(operatorPermissions),
};

export function policy(overrides: Partial<Policy> = {}): Policy {
  return {
    id: policyId,
    name: "每日备份",
    source_id: sourceId,
    local_repository_id: repositoryId,
    replica_repository_id: replicaRepositoryId,
    full_cron: "0 2 * * *",
    incremental_cron: "0 * * * *",
    timezone: "Asia/Shanghai",
    max_chain_length: 7,
    parallel: 1,
    throttle: 0,
    zstd_level: 3,
    page_tracking: false,
    enabled: true,
    last_preflight: null,
    created_at: createdAt,
    updated_at: createdAt,
    ...overrides,
  };
}

export function schedule(
  overrides: Partial<PolicySchedule> = {},
): PolicySchedule {
  return {
    policy_id: policyId,
    enabled: true,
    timezone: "Asia/Shanghai",
    fires: [
      { kind: "full", at: "2026-11-06T18:00:00Z", missed: true },
      { kind: "incremental", at: "2026-11-06T19:00:00Z", missed: false },
    ],
    ...overrides,
  };
}

export function run(overrides: Partial<Run> = {}): Run {
  return {
    id: runId,
    policy_id: policyId,
    source_id: sourceId,
    kind: "full",
    trigger: "manual",
    status: "running",
    scheduled_at: createdAt,
    created_at: createdAt,
    started_at: createdAt,
    finished_at: null,
    backup_id: null,
    chain_id: null,
    payload_size: null,
    error_class: null,
    error_code: null,
    cancel_requested: false,
    actor_id: user.id,
    ...overrides,
  };
}

export function runLog(overrides: Partial<RunLog> = {}): RunLog {
  return {
    run_id: runId,
    seq: 1,
    ts: createdAt,
    level: "info",
    event: "started",
    message: "backup started",
    data: null,
    ...overrides,
  };
}

export function replica(overrides: Partial<Replica> = {}): Replica {
  return {
    backup_id: backupId,
    repository_id: replicaRepositoryId,
    status: "failed",
    attempts: 3,
    next_attempt_at: "2026-11-06T09:00:00Z",
    uploaded_at: null,
    verified_at: null,
    alert_since: "2026-11-06T08:30:00Z",
    last_error_code: "ftp_refused",
    last_reply_code: 425,
    ...overrides,
  };
}

export function backup(overrides: Partial<Backup> = {}): Backup {
  return {
    id: backupId,
    chain_id: chainId,
    parent_id: "",
    kind: "physical-full",
    status: "completed",
    seq: 1,
    from_lsn: "",
    to_lsn: "1000",
    last_lsn: "1000",
    object_path: "backups/full.xb",
    payload_size: 4096,
    payload_sha256: "ab".repeat(32),
    local_present: true,
    completed_at: createdAt,
    replicas: [replica()],
    ...overrides,
  };
}

export function chain(overrides: Partial<Chain> = {}): Chain {
  return {
    id: chainId,
    source_id: sourceId,
    server_uuid: "11111111-2222-3333-4444-555555555555",
    started_at: createdAt,
    sealed_at: createdAt,
    pinned: false,
    members: [backup()],
    ...overrides,
  };
}

export function point(overrides: Partial<Point> = {}): Point {
  return {
    backup_id: backupId,
    chain_id: chainId,
    at: "2026-11-05T08:00:00Z",
    ...overrides,
  };
}

export function protection(overrides: Partial<Protection> = {}): Protection {
  return {
    local: point(),
    remote: [
      point({
        repository_id: replicaRepositoryId,
        backup_id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a08",
        at: "2026-11-04T08:00:00Z",
      }),
    ],
    ...overrides,
  };
}

export const emptyList = { items: [], next_before: null };
