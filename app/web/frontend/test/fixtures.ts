import type {
  Backup,
  Chain,
  Permission,
  Point,
  Policy,
  PolicySchedule,
  Protection,
  Replica,
  Restore,
  RestoreLog,
  RestorePlan,
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
export const restoreId = "0190c1f64f2a7c1e9b556f0f8d1e2a09";
export const planDigest = "ab".repeat(32);

export function sessionBody(permissions: Permission[]) {
  return { user, permissions };
}

const viewerPermissions: Permission[] = [
  "session:self",
  "resources:read",
  "runs:read",
  "restores:read",
];

const operatorPermissions: Permission[] = [
  ...viewerPermissions,
  "runs:trigger",
  "runs:cancel",
  "replicas:retry",
  "restores:plan",
];

const adminPermissions: Permission[] = [
  ...operatorPermissions,
  "repositories:manage",
  "users:manage",
  "audit:read",
  "restores:execute",
];

export const sessions = {
  viewer: () => sessionBody(viewerPermissions),
  operator: () => sessionBody(operatorPermissions),
  admin: () => sessionBody(adminPermissions),
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
    // The chains endpoint never fills member replicas (see the contract
    // description of listSourceChains); pages read them elsewhere.
    members: [backup({ replicas: [] })],
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

export function restorePlan(overrides: Partial<RestorePlan> = {}): RestorePlan {
  return {
    backup_id: backupId,
    chain_id: chainId,
    source_id: sourceId,
    server_uuid: "11111111-2222-3333-4444-555555555555",
    server_version: "8.4.11",
    tool_version: "8.4.0-7",
    target_to_lsn: 2000,
    links: [
      {
        backup_id: backupId,
        seq: 1,
        kind: "full",
        from_lsn: 0,
        to_lsn: 1000,
        source: "local",
        repository_id: repositoryId,
        payload_size: 4096,
        metadata_size: 128,
      },
      {
        backup_id: "0190c1f6-4f2a-7c1e-9b55-6f0f8d1e2a08",
        seq: 2,
        kind: "incremental",
        from_lsn: 1000,
        to_lsn: 2000,
        source: "ftp",
        repository_id: replicaRepositoryId,
        payload_size: 512,
        metadata_size: 64,
      },
    ],
    required_work_bytes: 8192,
    required_export_bytes: 8192,
    available_work_bytes: 1048576,
    available_export_bytes: 2097152,
    export_path: "/var/lib/intern/export/{restore_id}",
    risks: [
      "no_auto_replace_production",
      "server_uuid_duplicate",
      "pitr_unavailable",
    ],
    plan_digest: planDigest,
    rehearsal_available: true,
    ...overrides,
  };
}

export function restoreLog(overrides: Partial<RestoreLog> = {}): RestoreLog {
  return {
    restore_id: restoreId,
    seq: 1,
    ts: createdAt,
    level: "info",
    message: "restore phase extracting",
    data: { phase: "extracting" },
    ...overrides,
  };
}

export function restore(overrides: Partial<Restore> = {}): Restore {
  return {
    id: restoreId,
    backup_id: backupId,
    status: "completed",
    phase: null,
    plan: restorePlan(),
    plan_digest: planDigest,
    rehearse: true,
    verify_sql: ["SELECT 1"],
    requested_by: user.id,
    work_dir: null,
    export_path: `/var/lib/intern/export/${restoreId}/data`,
    export_bytes: 8192,
    created_at: createdAt,
    started_at: createdAt,
    finished_at: createdAt,
    cancel_requested: false,
    error_class: null,
    error_code: null,
    side_effects: {
      work_dir_removed: true,
      pending_export_removed: true,
      export_published: true,
    },
    verify_status: "passed",
    verify_result: {
      status: "passed",
      started: true,
      system_tables_readable: true,
      schemas: [{ name: "app", tables: 1, rows: 2 }],
      check_table: [{ table: "app.records", status: "OK" }],
      queries: [
        { sql: "SELECT 1", columns: ["1"], rows: [["1"]] },
        {
          sql: "SELECT COUNT(*) FROM app.records",
          columns: ["COUNT(*)"],
          rows: [["2"]],
        },
      ],
    },
    ...overrides,
  };
}
