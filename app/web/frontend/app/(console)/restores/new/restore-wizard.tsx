"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { ClockIcon, CopyIcon, ShieldAlertIcon } from "lucide-react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { useRef, useState } from "react";
import { Diagnostics } from "@/components/resources/diagnostics";
import { EmptyChoiceField } from "@/components/resources/empty-choice-field";
import {
  PageHeader,
  ResourceError,
  UnavailableAlert,
} from "@/components/resources/states";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Empty,
  EmptyContent,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { toast } from "@/components/ui/toast";
import { useSession } from "@/hooks/use-session";
import { ApiError, isUnauthorized } from "@/lib/api/errors";
import {
  executeRestoreMutation,
  listReplicasOptions,
  planRestoreMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import type {
  Backup,
  Chain,
  Replica,
  RestorePlan,
} from "@/lib/api/generated/types.gen";
import { formatBytes, formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  useRepositoryChoices,
  useSourceChains,
  useSourceChoices,
} from "@/lib/resources/queries";
import {
  describeRestoreError,
  exportLocation,
  isConfirmationFailed,
  isPlanChanged,
  isRateLimited,
  isRestoreRefused,
  linkSourceLabel,
  parseVerifySql,
  restoreValidationFields,
  riskText,
  shortenDigest,
} from "@/lib/resources/restore";
import { backupKindText } from "@/lib/resources/runs";

export function RestoreWizard() {
  const presetBackupId = useSearchParams().get("backup_id");
  const session = useSession();
  const canPlan = hasPermission(session.data?.permissions, "restores:plan");
  const canExecute = hasPermission(
    session.data?.permissions,
    "restores:execute",
  );
  const sources = useSourceChoices();
  const [sourceId, setSourceId] = useState("");
  const [chainId, setChainId] = useState("");
  const [backupId, setBackupId] = useState(presetBackupId ?? "");
  const chains = useSourceChains(sourceId === "" ? null : sourceId);

  if (!canPlan && session.isSuccess) {
    return (
      <Alert>
        <ShieldAlertIcon />
        <AlertTitle>只有操作员或管理员可以计划恢复</AlertTitle>
        <AlertDescription>
          当前账号可以查看恢复记录，但不能生成恢复计划。
        </AlertDescription>
      </Alert>
    );
  }

  const sourceLabels = Object.fromEntries(
    (sources.data?.items ?? []).map((source) => [source.id, source.name]),
  );
  const chainItems = chains.data?.items ?? [];
  const effectiveChainId = chainItems.some((chain) => chain.id === chainId)
    ? chainId
    : chainItems.length === 1
      ? (chainItems[0]?.id ?? "")
      : "";
  const selectedChain = chainItems.find(
    (chain) => chain.id === effectiveChainId,
  );
  const selectedBackup = selectedChain?.members.find(
    (member) => member.id === backupId,
  );

  return (
    <div className="flex w-full max-w-4xl flex-col gap-5">
      <PageHeader
        title="新建恢复"
        description="选择一个已完成的备份点，查看只读计划，再由管理员确认执行。恢复不会替换生产实例。"
      />

      <Card>
        <CardHeader>
          <CardTitle>1 · 选择恢复范围</CardTitle>
          <CardDescription>
            选择一次已完成的备份，恢复到隔离的导出目录。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <RadioGroup aria-label="恢复方式" value="checkpoint">
            <div className="flex items-start gap-2 text-sm">
              <RadioGroupItem
                value="checkpoint"
                aria-label="恢复到某次已完成的备份"
              />
              <span>恢复到某次已完成的备份</span>
            </div>
            <div className="flex items-start gap-2 text-sm">
              <RadioGroupItem
                value="pitr"
                disabled
                aria-label="任意时间点恢复（暂不支持）"
              />
              <div className="flex flex-col gap-1">
                <span className="text-muted-foreground">
                  任意时间点恢复（暂不支持）
                </span>
                <span className="text-muted-foreground">
                  当前版本只能恢复到已完成的备份时间点。
                </span>
              </div>
            </div>
          </RadioGroup>

          {sources.isSuccess && (sources.data?.items.length ?? 0) === 0 ? (
            <EmptyChoiceField
              id="restore-source"
              label="数据源"
              message="还没有可恢复的备份"
              placeholder="暂无可选数据源"
              href="/policies"
              action="查看保护策略"
            />
          ) : (
            <Field>
              <FieldLabel htmlFor="restore-source">数据源</FieldLabel>
              <Select
                items={sourceLabels}
                value={sourceId === "" ? null : sourceId}
                onValueChange={(value) => {
                  setSourceId(value as string);
                  setChainId("");
                  setBackupId("");
                }}
              >
                <SelectTrigger id="restore-source" className="w-full">
                  <SelectValue placeholder="请选择数据源" />
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    {(sources.data?.items ?? []).map((source) => (
                      <SelectItem key={source.id} value={source.id}>
                        {source.name}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
              {sources.isError ? (
                <ResourceError
                  title="无法读取数据源"
                  error={sources.error}
                  onRetry={() => sources.refetch()}
                />
              ) : null}
            </Field>
          )}

          {sourceId !== "" ? (
            <Field>
              <FieldLabel>可恢复的备份</FieldLabel>
              {chains.isPending ? <Skeleton className="h-16 w-full" /> : null}
              {chains.isError ? (
                <ResourceError
                  title="无法读取备份列表"
                  error={chains.error}
                  onRetry={() => chains.refetch()}
                />
              ) : null}
              {chains.isSuccess && chainItems.length === 0 ? (
                <Empty className="border">
                  <EmptyHeader>
                    <EmptyTitle className="text-sm">
                      这个数据源还没有可恢复的备份
                    </EmptyTitle>
                    <EmptyContent>
                      <Button
                        nativeButton={false}
                        role="link"
                        variant="outline"
                        size="sm"
                        render={<Link href="/policies" />}
                      >
                        查看保护策略
                      </Button>
                    </EmptyContent>
                  </EmptyHeader>
                </Empty>
              ) : null}
              {chainItems.length > 1 ? (
                <RadioGroup
                  aria-label="可恢复的备份分组"
                  value={effectiveChainId}
                  onValueChange={(value) => {
                    setChainId(value as string);
                    setBackupId("");
                  }}
                >
                  {chainItems.map((chain) => (
                    <div
                      key={chain.id}
                      className="flex items-center gap-2 text-sm"
                    >
                      <RadioGroupItem
                        value={chain.id}
                        aria-label={`备份组：自 ${formatDateTime(chain.started_at)} 全量起`}
                      />
                      <span>
                        备份组：自 {formatDateTime(chain.started_at)} 全量起
                      </span>
                    </div>
                  ))}
                </RadioGroup>
              ) : null}
              {chainItems.length === 1 && selectedChain ? (
                <p className="text-sm text-muted-foreground">
                  备份组：自 {formatDateTime(selectedChain.started_at)} 全量起
                </p>
              ) : null}
            </Field>
          ) : null}

          {selectedChain ? (
            <BackupPoints
              chain={selectedChain}
              backupId={backupId}
              onSelect={setBackupId}
            />
          ) : null}
        </CardContent>
      </Card>

      {selectedBackup ? (
        <PlanStep backup={selectedBackup} canExecute={canExecute} />
      ) : null}
    </div>
  );
}

function BackupPoints({
  chain,
  backupId,
  onSelect,
}: {
  chain: Chain;
  backupId: string;
  onSelect: (id: string) => void;
}) {
  const repositories = useRepositoryChoices();
  const replicas = useQuery(
    listReplicasOptions({
      query: { source_id: chain.source_id, status: "verified", limit: 200 },
    }),
  );
  const verifiedBy = new Map<string, Replica[]>();
  for (const replica of replicas.data?.items ?? []) {
    verifiedBy.set(replica.backup_id, [
      ...(verifiedBy.get(replica.backup_id) ?? []),
      replica,
    ]);
  }
  const names = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );
  return (
    <Field>
      <FieldLabel>2 · 选择备份点</FieldLabel>
      <FieldDescription>只能选择已完成的备份点。</FieldDescription>
      <RadioGroup
        aria-label="备份点"
        value={backupId}
        onValueChange={(value) => onSelect(value as string)}
      >
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>选择</TableHead>
              <TableHead>类型</TableHead>
              <TableHead>完成时间</TableHead>
              <TableHead>本地备份</TableHead>
              <TableHead>异地副本</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {chain.members.map((member) => {
              const completed = member.status === "completed";
              const verified = verifiedBy.get(member.id) ?? [];
              const when = formatDateTime(member.completed_at);
              const label = `${backupKindText(member.kind)} · ${when}`;
              return (
                <TableRow key={member.id}>
                  <TableCell>
                    <RadioGroupItem
                      value={member.id}
                      disabled={!completed}
                      aria-label={label}
                    />
                  </TableCell>
                  <TableCell>{backupKindText(member.kind)}</TableCell>
                  <TableCell>{when}</TableCell>
                  <TableCell>
                    {member.local_present ? "可用" : "已清理"}
                  </TableCell>
                  <TableCell>
                    {replicas.isPending
                      ? "读取中"
                      : replicas.isError
                        ? "无法读取"
                        : verified.length === 0
                          ? "无"
                          : verified
                              .map(
                                (replica) =>
                                  names.get(replica.repository_id) ??
                                  replica.repository_id,
                              )
                              .join("、")}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </RadioGroup>
    </Field>
  );
}

function PlanStep({
  backup,
  canExecute,
}: {
  backup: Backup;
  canExecute: boolean;
}) {
  const router = useRouter();
  const repositories = useRepositoryChoices();
  const [password, setPassword] = useState("");
  const [passwordInvalid, setPasswordInvalid] = useState(false);
  const [rehearse, setRehearse] = useState(false);
  const [rehearseError, setRehearseError] = useState<string | null>(null);
  const [verifySql, setVerifySql] = useState("");
  const [verifyError, setVerifyError] = useState<string | null>(null);
  const flightRef = useRef<{
    key: string;
    digest: string;
    backupId: string;
    pending: boolean;
  } | null>(null);
  const plan = useMutation(planRestoreMutation());
  const execute = useMutation(executeRestoreMutation());
  const repositoryNames = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );

  function requestPlan() {
    setPassword("");
    setPasswordInvalid(false);
    setVerifyError(null);
    setRehearseError(null);
    flightRef.current = null;
    execute.reset();
    plan.mutate({ body: { backup_id: backup.id } });
  }

  function submit(current: RestorePlan) {
    const existing = flightRef.current;
    const sameSubmission =
      existing !== null &&
      existing.digest === current.plan_digest &&
      existing.backupId === backup.id;
    if (sameSubmission && existing.pending) {
      return;
    }
    const parsed = rehearse
      ? parseVerifySql(verifySql)
      : { ok: true as const, statements: [] };
    if (!parsed.ok) {
      setVerifyError(parsed.message);
      return;
    }
    setVerifyError(null);
    const key = sameSubmission ? existing.key : crypto.randomUUID();
    flightRef.current = {
      key,
      digest: current.plan_digest,
      backupId: backup.id,
      pending: true,
    };
    const submitted = password;
    setPassword("");

    execute.mutate(
      {
        body: {
          backup_id: backup.id,
          plan_digest: current.plan_digest,
          password: submitted,
          rehearse,
          verify_sql: parsed.statements,
        },
        headers: { "Idempotency-Key": key },
      },
      {
        onSuccess: (accepted) => {
          flightRef.current = null;
          router.push(`/restores/detail?id=${accepted.restore_id}`);
        },
        onError: (error) => {
          if (isUnauthorized(error)) {
            return;
          }
          const fields = restoreValidationFields(error);
          setPasswordInvalid(isConfirmationFailed(error));
          setVerifyError(fields.verifySql ?? null);
          setRehearseError(fields.rehearse ?? null);
          const definitive =
            error instanceof ApiError &&
            error.status >= 400 &&
            error.status < 500;
          flightRef.current =
            definitive || flightRef.current === null
              ? null
              : { ...flightRef.current, pending: false };
        },
      },
    );
  }

  const current = plan.data;
  const planRefused = plan.isError && isRestoreRefused(plan.error);
  const executeNotice = executeNoticeOf(execute.error);

  return (
    <Card>
      <CardHeader>
        <CardTitle>3 · 恢复计划</CardTitle>
        <CardDescription>
          查看空间需求与导出位置，确认后开始隔离恢复。备份内容变化时需要重新生成计划。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div>
          <Button
            variant={current ? "outline" : "default"}
            disabled={plan.isPending}
            onClick={requestPlan}
          >
            {plan.isPending ? <Spinner data-icon="inline-start" /> : null}
            {current ? "重新生成计划" : "生成计划"}
          </Button>
        </div>

        {plan.isError &&
        plan.error instanceof ApiError &&
        plan.error.code === "unavailable" ? (
          <UnavailableAlert error={plan.error} />
        ) : null}
        {planRefused ? (
          <Alert variant="destructive">
            <ShieldAlertIcon />
            <AlertTitle>无法生成计划</AlertTitle>
            <AlertDescription>
              {describeRestoreError(plan.error)}
            </AlertDescription>
          </Alert>
        ) : null}
        {plan.isError &&
        !planRefused &&
        !(
          plan.error instanceof ApiError && plan.error.code === "unavailable"
        ) ? (
          <ResourceError title="无法生成计划" error={plan.error} />
        ) : null}
        {current ? (
          <PlanSummary plan={current} repositoryNames={repositoryNames} />
        ) : null}

        {current && !canExecute ? (
          <Alert>
            <ShieldAlertIcon />
            <AlertTitle>只有管理员可以执行恢复</AlertTitle>
            <AlertDescription>
              计划已生成，当前账号可以查看但不能确认执行。请让管理员打开本页，使用同一备份点重新生成计划后再执行。
            </AlertDescription>
          </Alert>
        ) : null}

        {current && canExecute ? (
          <ConfirmStep
            plan={current}
            password={password}
            passwordInvalid={passwordInvalid}
            rehearse={rehearse}
            rehearseError={rehearseError}
            verifySql={verifySql}
            verifyError={verifyError}
            pending={execute.isPending}
            notice={executeNotice}
            onPassword={(value) => {
              setPassword(value);
              setPasswordInvalid(false);
            }}
            onRehearse={setRehearse}
            onVerifySql={(value) => {
              setVerifySql(value);
              setVerifyError(null);
            }}
            onSubmit={() => submit(current)}
            onReplan={requestPlan}
          />
        ) : null}
      </CardContent>
    </Card>
  );
}

function executeNoticeOf(error: unknown): {
  kind: "plan_changed" | "rate" | "other";
  text: string;
} | null {
  if (error === null) {
    return null;
  }
  if (isPlanChanged(error)) {
    return {
      kind: "plan_changed",
      text: "目录已变化，原来确认的计划摘要已失效，不能用它再次提交。请重新生成计划，并重新输入密码确认。",
    };
  }
  if (isConfirmationFailed(error)) {
    return null;
  }
  if (isRateLimited(error)) {
    return { kind: "rate", text: describeRestoreError(error) };
  }
  if (error instanceof ApiError && error.code === "unavailable") {
    return null;
  }
  const fields = restoreValidationFields(error);
  if (fields.verifySql !== undefined || fields.rehearse !== undefined) {
    return null;
  }
  if (isRestoreRefused(error)) {
    return { kind: "other", text: describeRestoreError(error) };
  }
  return { kind: "other", text: describeRestoreError(error) };
}

function PlanSummary({
  plan,
  repositoryNames,
}: {
  plan: RestorePlan;
  repositoryNames: Map<string, string>;
}) {
  const location = exportLocation(plan.export_path);
  return (
    <div className="flex flex-col gap-4">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
        <dt className="text-muted-foreground">恢复范围</dt>
        <dd>{plan.links.length} 个备份点</dd>
        <dt className="text-muted-foreground">工作空间</dt>
        <dd>
          需要 {formatBytes(plan.required_work_bytes)}，可用{" "}
          {formatBytes(plan.available_work_bytes)}
        </dd>
        <dt className="text-muted-foreground">导出空间</dt>
        <dd>
          需要 {formatBytes(plan.required_export_bytes)}，可用{" "}
          {formatBytes(plan.available_export_bytes)}
        </dd>
        <dt className="text-muted-foreground">导出位置</dt>
        <dd className="break-all">
          <span className="font-mono text-xs">{location.root}</span>
          {location.pending ? (
            <span className="text-muted-foreground"> · 将新建恢复目录</span>
          ) : null}
        </dd>
        <dt className="text-muted-foreground">演练</dt>
        <dd>{plan.rehearsal_available ? "可用" : "不可用"}</dd>
      </dl>
      <Diagnostics>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
          <dt className="text-muted-foreground">目标 LSN</dt>
          <dd className="font-mono text-xs">{plan.target_to_lsn}</dd>
          <dt className="text-muted-foreground">计划摘要</dt>
          <dd className="flex items-center gap-2">
            <span className="font-mono text-xs" title={plan.plan_digest}>
              {shortenDigest(plan.plan_digest)}
            </span>
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                void navigator.clipboard.writeText(plan.plan_digest).then(
                  () => toast.add({ type: "success", title: "已复制计划摘要" }),
                  () =>
                    toast.add({
                      type: "error",
                      title: "无法复制计划摘要",
                    }),
                );
              }}
            >
              <CopyIcon data-icon="inline-start" />
              复制
            </Button>
          </dd>
        </dl>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>序号</TableHead>
              <TableHead>类型</TableHead>
              <TableHead>来源</TableHead>
              <TableHead>仓库</TableHead>
              <TableHead>数据量</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {plan.links.map((link) => (
              <TableRow key={link.backup_id}>
                <TableCell>{link.seq}</TableCell>
                <TableCell>{link.kind === "full" ? "全量" : "增量"}</TableCell>
                <TableCell>{linkSourceLabel[link.source]}</TableCell>
                <TableCell>
                  {link.repository_id === ""
                    ? "—"
                    : (repositoryNames.get(link.repository_id) ??
                      link.repository_id)}
                </TableCell>
                <TableCell>
                  {formatBytes(link.payload_size)} +{" "}
                  {formatBytes(link.metadata_size)} 元数据
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
          <dt className="text-muted-foreground">风险代码</dt>
          <dd className="flex flex-wrap gap-2 font-mono text-xs break-all">
            {plan.risks.map((code) => (
              <span key={code}>{code}</span>
            ))}
          </dd>
        </dl>
      </Diagnostics>

      <div className="flex flex-col gap-2">
        <p className="text-sm font-medium">风险</p>
        {plan.risks.map((code) => (
          <Alert key={code}>
            <ShieldAlertIcon />
            <AlertTitle>{riskText(code)}</AlertTitle>
          </Alert>
        ))}
      </div>
    </div>
  );
}

function ConfirmStep({
  plan,
  password,
  passwordInvalid,
  rehearse,
  rehearseError,
  verifySql,
  verifyError,
  pending,
  notice,
  onPassword,
  onRehearse,
  onVerifySql,
  onSubmit,
  onReplan,
}: {
  plan: RestorePlan;
  password: string;
  passwordInvalid: boolean;
  rehearse: boolean;
  rehearseError: string | null;
  verifySql: string;
  verifyError: string | null;
  pending: boolean;
  notice: { kind: "plan_changed" | "rate" | "other"; text: string } | null;
  onPassword: (value: string) => void;
  onRehearse: (value: boolean) => void;
  onVerifySql: (value: string) => void;
  onSubmit: () => void;
  onReplan: () => void;
}) {
  const rehearsalDisabled = !plan.rehearsal_available;
  return (
    <div className="flex flex-col gap-4 border-t pt-4">
      <div>
        <p className="text-sm font-medium">4 · 确认执行</p>
        <p className="text-sm text-muted-foreground">
          需要重新输入管理员密码。密码只用于这一次确认，不会被保存，提交后立即从输入框清除。
        </p>
      </div>

      {notice?.kind === "plan_changed" ? (
        <Alert variant="destructive">
          <ClockIcon />
          <AlertTitle>目录已变化</AlertTitle>
          <AlertDescription className="flex flex-col items-start gap-3">
            {notice.text}
            <Button variant="outline" size="sm" onClick={onReplan}>
              重新生成计划
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {notice && notice.kind !== "plan_changed" ? (
        <Alert variant="destructive">
          <ShieldAlertIcon />
          <AlertTitle>执行失败</AlertTitle>
          <AlertDescription>{notice.text}</AlertDescription>
        </Alert>
      ) : null}

      <Field data-invalid={passwordInvalid ? true : undefined}>
        <FieldLabel htmlFor="restore-password">管理员密码</FieldLabel>
        <Input
          id="restore-password"
          type="password"
          autoComplete="current-password"
          value={password}
          aria-invalid={passwordInvalid ? true : undefined}
          onChange={(event) => onPassword(event.target.value)}
        />
        <FieldDescription>
          密码错误不会开始恢复；多次错误会被暂时限制。
        </FieldDescription>
        {passwordInvalid ? (
          <FieldError>密码确认失败，请重新输入。</FieldError>
        ) : null}
      </Field>

      <Field data-invalid={rehearseError ? true : undefined}>
        <div className="flex items-center gap-2 text-sm">
          <Switch
            aria-label="执行演练"
            checked={rehearse && !rehearsalDisabled}
            disabled={rehearsalDisabled}
            onCheckedChange={onRehearse}
          />
          <span>在隔离实例上演练并执行校验语句</span>
        </div>
        {rehearsalDisabled ? (
          <FieldDescription>
            当前部署没有配置演练用的数据库程序，演练不可用。恢复本身仍可执行。
          </FieldDescription>
        ) : (
          <FieldDescription>
            恢复完成后启动临时数据库检查数据，演练失败不影响已导出的数据。
          </FieldDescription>
        )}
        {rehearseError ? <FieldError>{rehearseError}</FieldError> : null}
      </Field>

      {rehearse ? (
        <Field data-invalid={verifyError ? true : undefined}>
          <FieldLabel htmlFor="restore-verify-sql">校验语句</FieldLabel>
          <Textarea
            id="restore-verify-sql"
            value={verifySql}
            aria-invalid={verifyError ? true : undefined}
            onChange={(event) => onVerifySql(event.target.value)}
            placeholder="每行一条只读 SELECT，可留空"
            className="font-mono"
            spellCheck={false}
          />
          <FieldDescription>最多 16 条只读查询，可留空。</FieldDescription>
          {verifyError ? <FieldError>{verifyError}</FieldError> : null}
        </Field>
      ) : null}

      <div>
        <Button
          disabled={
            pending || notice?.kind === "plan_changed" || password === ""
          }
          onClick={onSubmit}
        >
          {pending ? <Spinner data-icon="inline-start" /> : null}
          执行恢复
        </Button>
      </div>
    </div>
  );
}
