import { z } from "zod";
import type { Policy, PolicyWrite } from "@/lib/api/generated/types.gen";
import {
  describeFull,
  describeIncremental,
  fullCron,
  incrementalCron,
  parseFullCron,
  parseIncrementalCron,
} from "./schedule";
import { hasControlCharacters } from "./validation";

export type PolicyFormValues = {
  name: string;
  source_id: string;
  local_repository_id: string;
  replica_repository_id: string;
  full_cron: string;
  incremental_cron: string;
  timezone: string;
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

export function newPolicyFormValues(): PolicyFormValues {
  return {
    name: "",
    source_id: "",
    local_repository_id: "",
    replica_repository_id: "",
    full_cron: fullCron({
      frequency: "daily",
      time: "02:00",
      weekday: "1",
      monthDay: "1",
      cron: "0 2 * * *",
      legacy: false,
    }),
    incremental_cron: incrementalCron({
      frequency: "hourly",
      time: "00:00",
      everyHours: "1",
      cron: "0 * * * *",
      legacy: false,
    }),
    timezone: "Asia/Shanghai",
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
    enabled: value.enabled,
  };
}

/**
 * One line of business wording for a policy's two schedules, plus the raw
 * cron expression wherever the wording falls back to a custom schedule.
 */
export function scheduleSummary(
  policy: Pick<Policy, "full_cron" | "incremental_cron">,
): { text: string; expressions: string[] } {
  const full = parseFullCron(policy.full_cron);
  const incremental = parseIncrementalCron(policy.incremental_cron);
  return {
    text: `${full.frequency === "custom" ? "自定义全量安排" : describeFull(full)}；${incremental.frequency === "custom" ? "自定义增量安排" : describeIncremental(incremental)}`,
    expressions: [full, incremental]
      .filter((schedule) => schedule.frequency === "custom")
      .map((schedule) => schedule.cron),
  };
}
