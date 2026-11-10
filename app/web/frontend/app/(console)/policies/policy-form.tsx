"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import { EmptyChoiceField } from "@/components/resources/empty-choice-field";
import { FormActions, FormSection } from "@/components/resources/form-section";
import { PreflightResultView } from "@/components/resources/preflight-result";
import { ScheduleBuilder } from "@/components/resources/schedule-builder";
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
    <form
      noValidate
      onSubmit={onSubmit}
      className="flex w-full max-w-4xl flex-col gap-6"
    >
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

      <FormSection
        title="保护目标"
        description="选择要保护的数据源、本地仓库和可选异地副本。"
      >
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

          {sources.isSuccess && (sources.data?.items.length ?? 0) === 0 ? (
            <EmptyChoiceField
              id="policy-source"
              label="数据源"
              message="还没有数据源"
              placeholder="暂无可选数据源"
              href="/sources/new"
              action="添加数据源"
            />
          ) : (
            <Field data-invalid={errors.source_id ? true : undefined}>
              <FieldLabel htmlFor="policy-source">数据源</FieldLabel>
              <Select
                items={sourceLabels}
                value={values.source_id === "" ? null : values.source_id}
                onValueChange={(value) =>
                  updateValue("source_id", value as string)
                }
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
          )}

          <div className="grid gap-4 lg:grid-cols-2">
            {repositories.isSuccess && localRepositories.length === 0 ? (
              <EmptyChoiceField
                id="policy-local-repository"
                label="本地仓库"
                message="还没有本地仓库"
                placeholder="暂无可选本地仓库"
                href="/repositories/new"
                action="新建本地仓库"
              />
            ) : (
              <Field
                data-invalid={errors.local_repository_id ? true : undefined}
              >
                <FieldLabel htmlFor="policy-local-repository">
                  本地仓库
                </FieldLabel>
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
                  <SelectTrigger
                    id="policy-local-repository"
                    className="w-full"
                  >
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
                <FieldDescription>备份首先保存在这里。</FieldDescription>
                <FieldError>{errors.local_repository_id}</FieldError>
              </Field>
            )}

            <Field
              data-invalid={errors.replica_repository_id ? true : undefined}
            >
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
                <SelectTrigger
                  id="policy-replica-repository"
                  className="w-full"
                >
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
              <FieldDescription>为备份增加一份异地保护。</FieldDescription>
              <FieldError>{errors.replica_repository_id}</FieldError>
            </Field>
          </div>

          {creating ? (
            <Field>
              <div className="flex items-center gap-2 text-sm">
                <Switch
                  aria-label="创建后立即启用"
                  checked={values.enabled}
                  onCheckedChange={(checked) => updateValue("enabled", checked)}
                />
                <span>创建后立即启用</span>
              </div>
              <FieldDescription>
                启用前检查连接与备份环境，也可以先保存后启用。
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
      </FormSection>

      <ScheduleBuilder
        full={values.full_cron}
        incremental={values.incremental_cron}
        timezone={values.timezone}
        errors={{
          full_cron: errors.full_cron,
          incremental_cron: errors.incremental_cron,
          timezone: errors.timezone,
        }}
        onFullChange={(cron) => updateValue("full_cron", cron)}
        onIncrementalChange={(cron) => updateValue("incremental_cron", cron)}
        onTimezoneChange={(timezone) => updateValue("timezone", timezone)}
      />

      <FormActions>
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
      </FormActions>
    </form>
  );
}
