"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import { PreflightResultView } from "@/components/resources/preflight-result";
import { ResourceError, UnavailableAlert } from "@/components/resources/states";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { toast } from "@/components/ui/toast";
import {
  createPolicyMutation,
  updatePolicyMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import type { Policy } from "@/lib/api/generated/types.gen";
import { zPolicyWrite } from "@/lib/api/generated/zod.gen";
import {
  apiFieldErrors,
  describeResourceError,
  isUnavailable,
  preflightOf,
} from "@/lib/resources/errors";
import {
  buildPolicyWrite,
  newPolicyFormValues,
  type PolicyFormValues,
  policyFormSchema,
  policyToFormValues,
} from "@/lib/resources/policy";
import {
  invalidatePolicies,
  useRepositoryChoices,
  useSourceChoices,
} from "@/lib/resources/queries";
import { type FieldErrors, fieldErrors } from "@/lib/schemas";

const fields = [
  "name",
  "source_id",
  "local_repository_id",
  "replica_repository_id",
  "full_cron",
  "incremental_cron",
  "timezone",
  "max_chain_length",
  "parallel",
  "throttle",
  "zstd_level",
  "page_tracking",
  "enabled",
] as const;

export function PolicyForm({ policy }: { policy?: Policy }) {
  const creating = policy === undefined;
  const router = useRouter();
  const queryClient = useQueryClient();
  const sources = useSourceChoices();
  const repositories = useRepositoryChoices();
  const [values, setValues] = useState<PolicyFormValues>(() =>
    policy === undefined ? newPolicyFormValues() : policyToFormValues(policy),
  );
  const [errors, setErrors] = useState<FieldErrors<PolicyFormValues>>({});
  const [formError, setFormError] = useState<unknown>(null);

  const create = useMutation(createPolicyMutation());
  const update = useMutation(updatePolicyMutation());
  const mutation = creating ? create : update;
  const localRepositories =
    repositories.data?.items.filter((item) => item.kind === "local") ?? [];
  const replicaRepositories =
    repositories.data?.items.filter((item) => item.kind === "ftp") ?? [];
  // Base UI shows the raw value in a closed trigger unless the labels are known.
  const sourceLabels = Object.fromEntries(
    (sources.data?.items ?? []).map((source) => [source.id, source.name]),
  );
  const localRepositoryLabels = Object.fromEntries(
    localRepositories.map((repository) => [repository.id, repository.name]),
  );
  const replicaRepositoryLabels = {
    none: "不使用副本",
    ...Object.fromEntries(
      replicaRepositories.map((repository) => [repository.id, repository.name]),
    ),
  };

  function updateValue<K extends keyof PolicyFormValues>(
    key: K,
    value: PolicyFormValues[K],
  ) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = policyFormSchema().safeParse(values);
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      setFormError(null);
      return;
    }
    setErrors({});
    setFormError(null);
    const body = zPolicyWrite.safeParse(buildPolicyWrite(values));
    if (!body.success) {
      setErrors(fieldErrors(body.error));
      return;
    }
    const onSuccess = (saved: Policy) => {
      invalidatePolicies(queryClient, saved.id);
      toast.add({
        type: "success",
        title: creating ? "策略已创建" : "策略已更新",
        description: saved.name,
      });
      router.push(`/policies/detail?id=${saved.id}`);
    };
    const onError = (error: unknown) => {
      const mapped = apiFieldErrors(error, fields);
      if (Object.keys(mapped).length > 0) {
        setErrors(mapped);
        return;
      }
      if (preflightOf(error) !== null) {
        if (policy !== undefined) {
          invalidatePolicies(queryClient, policy.id);
        }
        return;
      }
      setFormError(error);
    };
    if (policy === undefined) {
      create.mutate({ body: body.data }, { onSuccess, onError });
      return;
    }
    update.mutate(
      { path: { id: policy.id }, body: body.data },
      { onSuccess, onError },
    );
  }

  const preflight = preflightOf(mutation.error);

  return (
    <form noValidate onSubmit={onSubmit} className="flex flex-col gap-6">
      {preflight ? (
        <PreflightResultView result={preflight} title="保存被预检拒绝" />
      ) : null}
      {formError ? (
        isUnavailable(formError) ? (
          <UnavailableAlert error={formError} />
        ) : (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>保存失败</AlertTitle>
            <AlertDescription>
              {describeResourceError(formError)}
            </AlertDescription>
          </Alert>
        )
      ) : null}
      {sources.isError ? (
        <ResourceError title="无法读取数据源" error={sources.error} />
      ) : null}
      {repositories.isError ? (
        <ResourceError title="无法读取仓库" error={repositories.error} />
      ) : null}

      <FieldGroup>
        <Field data-invalid={errors.name ? true : undefined}>
          <FieldLabel htmlFor="policy-name">名称</FieldLabel>
          <Input
            id="policy-name"
            value={values.name}
            onChange={(event) => updateValue("name", event.target.value)}
            aria-invalid={errors.name ? true : undefined}
            autoComplete="off"
          />
          <FieldDescription>1–64 个字符。</FieldDescription>
          <FieldError>{errors.name}</FieldError>
        </Field>

        <Field data-invalid={errors.source_id ? true : undefined}>
          <FieldLabel htmlFor="policy-source">数据源</FieldLabel>
          <Select
            items={sourceLabels}
            value={values.source_id === "" ? null : values.source_id}
            onValueChange={(value) => updateValue("source_id", value as string)}
          >
            <SelectTrigger id="policy-source" className="w-full">
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
          <FieldError>{errors.source_id}</FieldError>
        </Field>

        <Field data-invalid={errors.local_repository_id ? true : undefined}>
          <FieldLabel htmlFor="policy-local-repository">本地仓库</FieldLabel>
          <Select
            items={localRepositoryLabels}
            value={
              values.local_repository_id === ""
                ? null
                : values.local_repository_id
            }
            onValueChange={(value) =>
              updateValue("local_repository_id", value as string)
            }
          >
            <SelectTrigger id="policy-local-repository" className="w-full">
              <SelectValue placeholder="请选择本地仓库" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                {localRepositories.map((repository) => (
                  <SelectItem key={repository.id} value={repository.id}>
                    {repository.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          <FieldDescription>只能选择类型为本地目录的仓库。</FieldDescription>
          <FieldError>{errors.local_repository_id}</FieldError>
        </Field>

        <Field data-invalid={errors.replica_repository_id ? true : undefined}>
          <FieldLabel htmlFor="policy-replica-repository">
            副本仓库（可选）
          </FieldLabel>
          <Select
            items={replicaRepositoryLabels}
            value={values.replica_repository_id || "none"}
            onValueChange={(value) =>
              updateValue(
                "replica_repository_id",
                value === "none" ? "" : (value as string),
              )
            }
          >
            <SelectTrigger id="policy-replica-repository" className="w-full">
              <SelectValue placeholder="不使用副本" />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="none">不使用副本</SelectItem>
                {replicaRepositories.map((repository) => (
                  <SelectItem key={repository.id} value={repository.id}>
                    {repository.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          <FieldDescription>
            只能选择 FTP 或 FTPS 仓库，且必须与本地仓库不同。
          </FieldDescription>
          <FieldError>{errors.replica_repository_id}</FieldError>
        </Field>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field data-invalid={errors.full_cron ? true : undefined}>
            <FieldLabel htmlFor="policy-full-cron">全量 cron</FieldLabel>
            <Input
              id="policy-full-cron"
              value={values.full_cron}
              onChange={(event) => updateValue("full_cron", event.target.value)}
              aria-invalid={errors.full_cron ? true : undefined}
              className="font-mono"
              autoComplete="off"
            />
            <FieldError>{errors.full_cron}</FieldError>
          </Field>
          <Field data-invalid={errors.incremental_cron ? true : undefined}>
            <FieldLabel htmlFor="policy-incremental-cron">增量 cron</FieldLabel>
            <Input
              id="policy-incremental-cron"
              value={values.incremental_cron}
              onChange={(event) =>
                updateValue("incremental_cron", event.target.value)
              }
              aria-invalid={errors.incremental_cron ? true : undefined}
              className="font-mono"
              autoComplete="off"
            />
            <FieldError>{errors.incremental_cron}</FieldError>
          </Field>
        </div>

        <Field data-invalid={errors.timezone ? true : undefined}>
          <FieldLabel htmlFor="policy-timezone">时区</FieldLabel>
          <Input
            id="policy-timezone"
            value={values.timezone}
            onChange={(event) => updateValue("timezone", event.target.value)}
            aria-invalid={errors.timezone ? true : undefined}
            placeholder="Asia/Shanghai"
            className="font-mono"
            autoComplete="off"
          />
          <FieldDescription>
            IANA 时区名，例如 Asia/Shanghai；服务端会再次校验。
          </FieldDescription>
          <FieldError>{errors.timezone}</FieldError>
        </Field>

        <div className="grid gap-4 sm:grid-cols-2">
          <Field data-invalid={errors.max_chain_length ? true : undefined}>
            <FieldLabel htmlFor="policy-max-chain-length">最大链长</FieldLabel>
            <Input
              id="policy-max-chain-length"
              type="number"
              min={1}
              value={values.max_chain_length}
              onChange={(event) =>
                updateValue("max_chain_length", event.target.value)
              }
              aria-invalid={errors.max_chain_length ? true : undefined}
            />
            <FieldError>{errors.max_chain_length}</FieldError>
          </Field>
          <Field data-invalid={errors.parallel ? true : undefined}>
            <FieldLabel htmlFor="policy-parallel">并行度</FieldLabel>
            <Input
              id="policy-parallel"
              type="number"
              min={1}
              value={values.parallel}
              onChange={(event) => updateValue("parallel", event.target.value)}
              aria-invalid={errors.parallel ? true : undefined}
            />
            <FieldError>{errors.parallel}</FieldError>
          </Field>
          <Field data-invalid={errors.throttle ? true : undefined}>
            <FieldLabel htmlFor="policy-throttle">限速（字节/秒）</FieldLabel>
            <Input
              id="policy-throttle"
              type="number"
              min={0}
              value={values.throttle}
              onChange={(event) => updateValue("throttle", event.target.value)}
              aria-invalid={errors.throttle ? true : undefined}
            />
            <FieldError>{errors.throttle}</FieldError>
          </Field>
          <Field data-invalid={errors.zstd_level ? true : undefined}>
            <FieldLabel htmlFor="policy-zstd-level">zstd 级别</FieldLabel>
            <Input
              id="policy-zstd-level"
              type="number"
              min={1}
              max={22}
              value={values.zstd_level}
              onChange={(event) =>
                updateValue("zstd_level", event.target.value)
              }
              aria-invalid={errors.zstd_level ? true : undefined}
            />
            <FieldDescription>1–22。</FieldDescription>
            <FieldError>{errors.zstd_level}</FieldError>
          </Field>
        </div>

        <Field>
          <div className="flex items-center gap-2 text-sm">
            <Switch
              aria-label="启用 page tracking"
              checked={values.page_tracking}
              onCheckedChange={(checked) =>
                updateValue("page_tracking", checked)
              }
            />
            <span>启用 page tracking（page_tracking）</span>
          </div>
          <FieldDescription>
            为增量备份启用页跟踪；是否可用取决于源端 MySQL 版本。
          </FieldDescription>
        </Field>

        {creating ? (
          <Field>
            <div className="flex items-center gap-2 text-sm">
              <Switch
                aria-label="创建后立即启用"
                checked={values.enabled}
                onCheckedChange={(checked) => updateValue("enabled", checked)}
              />
              <span>创建后立即启用（会先运行真实预检）</span>
            </div>
            <FieldDescription>
              预检失败时不会创建策略；也可以先保存为未启用，再在详情页启用。
            </FieldDescription>
          </Field>
        ) : (
          <Field>
            <p className="text-sm font-medium">启用状态</p>
            <p className="text-sm">
              {policy.enabled ? "已启用" : "未启用"}
              （启用或停用在详情页操作；已启用策略保存时会重新预检）
            </p>
          </Field>
        )}
      </FieldGroup>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner data-icon="inline-start" /> : null}
          {creating ? "创建策略" : "保存修改"}
        </Button>
        <Button
          nativeButton={false}
          variant="outline"
          render={
            <Link
              href={
                policy !== undefined
                  ? `/policies/detail?id=${policy.id}`
                  : "/policies"
              }
            />
          }
        >
          取消
        </Button>
      </div>
    </form>
  );
}
