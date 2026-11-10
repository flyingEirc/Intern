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

import {
  checkLabel,
  preflightCheckLabel,
  preflightWarningText,
  warningText,
} from "@/lib/resources/preflight";

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
