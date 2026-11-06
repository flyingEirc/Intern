"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { ClockIcon, CopyIcon, ShieldAlertIcon } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";
import { useRef, useState } from "react";
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
import { formatBytes, formatDateTime, formatLsn } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import {
  useRepositoryChoices,
  useSourceChains,
  useSourceChoices,
} from "@/lib/resources/queries";
import {
  describeRestoreError,
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
import { backupKindText, replicaStatusLabel } from "@/lib/resources/runs";

const pitrReason =
  "binlog 归档尚未实现（路线图 N1），目前只能恢复到链上已完成的检查点，不能恢复到任意时间点。";

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
  const selectedChain = chainItems.find((chain) => chain.id === chainId);
  const selectedBackup = selectedChain?.members.find(
    (member) => member.id === backupId,
  );

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="新建恢复"
        description="选择一个已完成的备份点，查看只读计划，再由管理员确认执行。恢复不会替换生产实例。"
      />

      <Card>
        <CardHeader>
          <CardTitle>恢复范围</CardTitle>
          <CardDescription>{pitrReason}</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <RadioGroup aria-label="恢复方式" value="checkpoint">
            <div className="flex items-start gap-2 text-sm">
              <RadioGroupItem
                value="checkpoint"
                aria-label="恢复到备份链上的检查点"
              />
              <span>恢复到备份链上的检查点</span>
            </div>
            <div className="flex items-start gap-2 text-sm">
              <RadioGroupItem
                value="pitr"
                disabled
                aria-label="任意时间点恢复（PITR）暂不可用"
              />
              <div className="flex flex-col gap-1">
                <span className="text-muted-foreground">
                  任意时间点恢复（PITR）暂不可用
                </span>
                <span className="text-muted-foreground">{pitrReason}</span>
              </div>
            </div>
          </RadioGroup>

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

          {sourceId !== "" ? (
            <Field>
              <FieldLabel>备份链</FieldLabel>
              {chains.isPending ? <Skeleton className="h-16 w-full" /> : null}
              {chains.isError ? (
                <ResourceError
                  title="无法读取备份链"
                  error={chains.error}
                  onRetry={() => chains.refetch()}
                />
              ) : null}
              {chains.isSuccess && chainItems.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  这个数据源还没有备份链。
                </p>
              ) : null}
              {chainItems.length > 0 ? (
                <RadioGroup
                  aria-label="备份链"
                  value={chainId}
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
                      <RadioGroupItem value={chain.id} aria-label={chain.id} />
                      <span className="font-mono text-xs">{chain.id}</span>
                      <span className="text-muted-foreground">
                        {chain.members.length} 个备份点 · 开始{" "}
                        {formatDateTime(chain.started_at)}
                      </span>
                    </div>
                  ))}
                </RadioGroup>
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
  // The chains contract carries no replica rows, so the verified copies come
  // from the replica queue of this source (one bounded read).
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
      <FieldLabel>备份点</FieldLabel>
      <FieldDescription>
        只能选择已完成的备份点。本地是否仍在、以及哪些仓库持有已校验副本，都来自目录。
      </FieldDescription>
      <RadioGroup
        aria-label="备份点"
        value={backupId}
        onValueChange={(value) => onSelect(value as string)}
      >
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>选择</TableHead>
              <TableHead>序号</TableHead>
              <TableHead>类型</TableHead>
              <TableHead>完成时间</TableHead>
              <TableHead>LSN</TableHead>
              <TableHead>本地</TableHead>
              <TableHead>已校验副本</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {chain.members.map((member) => {
              const completed = member.status === "completed";
              const verified = verifiedBy.get(member.id) ?? [];
              return (
                <TableRow key={member.id}>
                  <TableCell>
                    <RadioGroupItem
                      value={member.id}
                      disabled={!completed}
                      aria-label={`${backupKindText(member.kind)} ${member.seq}`}
                    />
                  </TableCell>
                  <TableCell>{member.seq}</TableCell>
                  <TableCell>{backupKindText(member.kind)}</TableCell>
                  <TableCell>{formatDateTime(member.completed_at)}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {formatLsn(member.from_lsn)} → {formatLsn(member.to_lsn)}
                  </TableCell>
                  <TableCell>
                    {member.local_present ? "存在" : "已不在"}
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
                                  `${names.get(replica.repository_id) ?? replica.repository_id}（${replicaStatusLabel[replica.status]}）`,
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
  const repositories = useRepositoryChoices();
  const repositoryNames = new Map(
    (repositories.data?.items ?? []).map((repository) => [
      repository.id,
      repository.name,
    ]),
  );
  const router = useRouter();
  const plan = useMutation(planRestoreMutation());
  const execute = useMutation(executeRestoreMutation());
  const [password, setPassword] = useState("");
  const [rehearse, setRehearse] = useState(false);
  const [verifySql, setVerifySql] = useState("");
  const [verifyError, setVerifyError] = useState<string | null>(null);
  const [passwordInvalid, setPasswordInvalid] = useState(false);
  const [rehearseError, setRehearseError] = useState<string | null>(null);
  // The submission in flight, kept in a ref so two activations dispatched
  // before React re-renders (a fast double click) share one request instead of
  // queueing two restores. `pending` blocks re-entry only while a request is
  // outstanding; the key is kept after an unknown outcome (network or 5xx) so
  // the retry replays the same restore, and dropped after a definitive answer.
  const flightRef = useRef<{
    key: string;
    digest: string;
    backupId: string;
    pending: boolean;
  } | null>(null);

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
    const parsed = parseVerifySql(verifySql);
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
          // A 4xx is a definitive refusal: nothing was queued, so the next
          // attempt gets its own key. Anything else may have been accepted,
          // so the retry keeps the key and the server replays it.
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
        <CardTitle>只读计划</CardTitle>
        <CardDescription>
          生成计划不写恢复记录、不写文件、也不访问
          FTP。执行时服务端会重新计算计划，摘要不一致会被拒绝。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div>
          <Button disabled={plan.isPending} onClick={requestPlan}>
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
  return (
    <div className="flex flex-col gap-4">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
        <dt className="text-muted-foreground">目标 LSN</dt>
        <dd className="font-mono text-xs">{plan.target_to_lsn}</dd>
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
        <dt className="text-muted-foreground">导出路径</dt>
        <dd className="font-mono text-xs break-all">{plan.export_path}</dd>
        <dt className="text-muted-foreground">演练</dt>
        <dd>{plan.rehearsal_available ? "可用" : "不可用"}</dd>
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
            <TableHead>LSN</TableHead>
            <TableHead>载荷</TableHead>
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
              <TableCell className="font-mono text-xs">
                {link.from_lsn} → {link.to_lsn}
              </TableCell>
              <TableCell>
                {formatBytes(link.payload_size)} +{" "}
                {formatBytes(link.metadata_size)} 元数据
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>

      <div className="flex flex-col gap-2">
        <p className="text-sm font-medium">风险</p>
        {plan.risks.map((code) => (
          <Alert key={code}>
            <ShieldAlertIcon />
            <AlertTitle className="font-mono text-xs">{code}</AlertTitle>
            <AlertDescription>{riskText(code)}</AlertDescription>
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
        <p className="text-sm font-medium">确认执行</p>
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
          密码错误会得到 403，且不会创建恢复记录。改密与恢复确认共用同一限速。
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
            当前部署没有配置
            mysqld，演练不可用。恢复本身仍可执行，只是不能在隔离实例上校验。
          </FieldDescription>
        ) : (
          <FieldDescription>
            演练在导出发布之后进行。演练失败不会让已发布的导出变成失败的恢复。
          </FieldDescription>
        )}
        {rehearseError ? <FieldError>{rehearseError}</FieldError> : null}
      </Field>

      <Field data-invalid={verifyError ? true : undefined}>
        <FieldLabel htmlFor="restore-verify-sql">
          校验语句（verify_sql）
        </FieldLabel>
        <Textarea
          id="restore-verify-sql"
          value={verifySql}
          aria-invalid={verifyError ? true : undefined}
          onChange={(event) => onVerifySql(event.target.value)}
          placeholder="每行一条只读 SELECT，可留空"
          className="font-mono"
          spellCheck={false}
        />
        <FieldDescription>
          与恢复引擎同一上限：最多 16 条，每条 1 到 4096 字节，不能包含 NUL
          或语句内部的分号（允许一个结尾分号），且必须以 SELECT
          开头。留空表示不附加校验语句。引擎仍是最终裁定。
        </FieldDescription>
        {verifyError ? <FieldError>{verifyError}</FieldError> : null}
      </Field>

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
