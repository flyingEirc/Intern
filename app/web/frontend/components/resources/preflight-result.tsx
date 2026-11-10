"use client";

import {
  CircleAlertIcon,
  CircleCheckIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { Diagnostics } from "@/components/resources/diagnostics";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import type {
  PreflightRecord,
  PreflightResult,
} from "@/lib/api/generated/types.gen";

/**
 * Business names for the preflight check codes the engine records. An unknown
 * code keeps a generic Chinese name and shows its raw code in diagnostics.
 */
export const preflightCheckLabel: Record<string, string> = {
  identity: "数据库身份",
  compatibility: "版本兼容",
  volume: "数据卷",
  mount: "挂载",
  privileges: "权限",
  datafiles: "数据文件",
  tables: "表",
  keyring: "加密密钥",
  page_tracking: "页跟踪",
  capacity: "可用空间",
};

/** Business wording for the advisory warnings the engine emits. */
export const preflightWarningText: Record<string, string> = {
  compatibility_candidate_unverified: "当前 MySQL/工具版本组合尚未完成正式验证",
  table_inventory_partial:
    "备份账号无法列出全部表，非 InnoDB 表可能需要更强锁并整表复制",
};

export function checkLabel(code: string): string {
  return preflightCheckLabel[code] ?? "其他检查项";
}

export function warningText(code: string, message: string): string {
  return preflightWarningText[code] ?? message;
}

/**
 * Every check, warning and failure the backend reported. `PreflightRecord` is
 * the persisted form (no failure), `PreflightResult` the live answer. Machine
 * codes stay inside the diagnostics fold.
 */
export function PreflightResultView({
  result,
  title = "预检结果",
}: {
  result: PreflightResult | PreflightRecord;
  title?: string;
}) {
  const failure = "error" in result ? result.error : undefined;
  const codes = [
    ...result.checks.filter((check) => !(check in preflightCheckLabel)),
    ...result.warnings
      .filter((warning) => !(warning.code in preflightWarningText))
      .map((warning) => `${warning.code}：${warning.message}`),
    ...(failure === undefined
      ? []
      : [`${failure.check} / ${failure.code}：${failure.message}`]),
  ];
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          {title}
          <Badge variant={result.ok ? "secondary" : "destructive"}>
            {result.ok ? "通过" : "未通过"}
          </Badge>
        </CardTitle>
        <CardDescription>由真实数据源检查产生，逐项列出。</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">检查项</h3>
          {result.checks.length === 0 ? (
            <p className="text-sm text-muted-foreground">后端未返回检查项。</p>
          ) : (
            <ul className="flex flex-col gap-1 text-sm">
              {result.checks.map((check) => (
                <li key={check} className="flex items-start gap-2">
                  <CircleCheckIcon className="mt-0.5 text-muted-foreground" />
                  <span>{checkLabel(check)}</span>
                </li>
              ))}
            </ul>
          )}
        </section>
        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">警告</h3>
          {result.warnings.length === 0 ? (
            <p className="text-sm text-muted-foreground">没有警告。</p>
          ) : (
            <ul className="flex flex-col gap-2 text-sm">
              {result.warnings.map((warning) => (
                <li
                  key={`${warning.code}:${warning.message}`}
                  className="flex items-start gap-2"
                >
                  <TriangleAlertIcon className="mt-0.5 text-muted-foreground" />
                  <span>{warningText(warning.code, warning.message)}</span>
                </li>
              ))}
            </ul>
          )}
        </section>
        {failure ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>失败：{checkLabel(failure.check)}</AlertTitle>
            <AlertDescription className="flex flex-col gap-1">
              <span>{failure.message}</span>
              {failure.missing && failure.missing.length > 0 ? (
                <span>
                  缺少：
                  <span className="font-mono">
                    {failure.missing.join("、")}
                  </span>
                </span>
              ) : null}
            </AlertDescription>
          </Alert>
        ) : null}
        {codes.length > 0 ? (
          <Diagnostics
            title="预检诊断"
            description="排查时再展开检查项与警告的原始代码。"
          >
            <ul className="flex flex-col gap-1 font-mono text-xs break-all">
              {codes.map((code) => (
                <li key={code}>{code}</li>
              ))}
            </ul>
          </Diagnostics>
        ) : null}
      </CardContent>
    </Card>
  );
}
