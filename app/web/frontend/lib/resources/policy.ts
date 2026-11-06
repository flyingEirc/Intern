import { z } from "zod";
import type { Policy, PolicyWrite } from "@/lib/api/generated/types.gen";
import { hasControlCharacters } from "./validation";

export type PolicyFormValues = {
  name: string;
  source_id: string;
  local_repository_id: string;
  replica_repository_id: string;
  full_cron: string;
  incremental_cron: string;
  timezone: string;
  max_chain_length: string;
  parallel: string;
  throttle: string;
  zstd_level: string;
  page_tracking: boolean;
  enabled: boolean;
};

const cronExpression = (label: string) =>
  z
    .string()
    .trim()
    .min(1, `请输入${label}`)
    .refine(
      (value) => value.split(/\s+/).length === 5,
      `${label}必须是标准 5 段 cron 表达式`,
    );

const integerField = (label: string, minimum: number, maximum?: number) =>
  z
    .string()
    .trim()
    .min(1, `请输入${label}`)
    .refine(
      (value) =>
        /^\d+$/.test(value) &&
        Number(value) >= minimum &&
        (maximum === undefined || Number(value) <= maximum),
      maximum === undefined
        ? `${label}必须是不小于 ${minimum} 的整数`
        : `${label}必须是 ${minimum}–${maximum} 的整数`,
    );

export function newPolicyFormValues(): PolicyFormValues {
  return {
    name: "",
    source_id: "",
    local_repository_id: "",
    replica_repository_id: "",
    full_cron: "0 2 * * *",
    incremental_cron: "0 * * * *",
    timezone: "Asia/Shanghai",
    max_chain_length: "7",
    parallel: "1",
    throttle: "0",
    zstd_level: "3",
    page_tracking: false,
    enabled: false,
  };
}

export function policyToFormValues(policy: Policy): PolicyFormValues {
  return {
    name: policy.name,
    source_id: policy.source_id,
    local_repository_id: policy.local_repository_id,
    replica_repository_id: policy.replica_repository_id ?? "",
    full_cron: policy.full_cron,
    incremental_cron: policy.incremental_cron,
    timezone: policy.timezone,
    max_chain_length: String(policy.max_chain_length),
    parallel: String(policy.parallel),
    throttle: String(policy.throttle),
    zstd_level: String(policy.zstd_level),
    page_tracking: policy.page_tracking,
    enabled: policy.enabled,
  };
}

/** Form rules follow PolicyWrite; the backend remains the schedule authority. */
export function policyFormSchema() {
  return z
    .object({
      name: z
        .string()
        .trim()
        .min(1, "请输入策略名称")
        .max(64, "策略名称不能超过 64 个字符"),
      source_id: z.string().min(1, "请选择数据源"),
      local_repository_id: z.string().min(1, "请选择本地仓库"),
      replica_repository_id: z.string(),
      full_cron: cronExpression("全量 cron"),
      incremental_cron: cronExpression("增量 cron"),
      timezone: z
        .string()
        .trim()
        .min(1, "请输入 IANA 时区，例如 Asia/Shanghai")
        .refine(
          (value) => value !== "Local",
          "不能使用 Local，请输入 IANA 时区名",
        ),
      max_chain_length: integerField("最大链长", 1),
      parallel: integerField("并行度", 1),
      throttle: integerField("限速（字节/秒）", 0),
      zstd_level: integerField("zstd 级别", 1, 22),
      page_tracking: z.boolean(),
      enabled: z.boolean(),
    })
    .superRefine((value, ctx) => {
      if (hasControlCharacters(value.name)) {
        ctx.addIssue({
          code: "custom",
          path: ["name"],
          message: "策略名称不能包含控制字符",
        });
      }
      if (
        value.replica_repository_id !== "" &&
        value.replica_repository_id === value.local_repository_id
      ) {
        ctx.addIssue({
          code: "custom",
          path: ["replica_repository_id"],
          message: "副本仓库必须与本地仓库不同",
        });
      }
    });
}

export function buildPolicyWrite(value: PolicyFormValues): PolicyWrite {
  return {
    name: value.name.trim(),
    source_id: value.source_id,
    local_repository_id: value.local_repository_id,
    replica_repository_id:
      value.replica_repository_id === "" ? null : value.replica_repository_id,
    full_cron: value.full_cron.trim(),
    incremental_cron: value.incremental_cron.trim(),
    timezone: value.timezone.trim(),
    max_chain_length: Number(value.max_chain_length),
    parallel: Number(value.parallel),
    throttle: Number(value.throttle),
    zstd_level: Number(value.zstd_level),
    page_tracking: value.page_tracking,
    enabled: value.enabled,
  };
}
