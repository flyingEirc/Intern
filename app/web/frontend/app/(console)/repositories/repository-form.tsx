"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import { PlaintextWarning } from "@/components/resources/plaintext-warning";
import { PreflightResultView } from "@/components/resources/preflight-result";
import { UnavailableAlert } from "@/components/resources/states";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { toast } from "@/components/ui/toast";
import {
  createRepositoryMutation,
  updateRepositoryMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import type {
  Repository,
  RepositoryKind,
  TlsMode,
} from "@/lib/api/generated/types.gen";
import { zRepositoryWriteWritable } from "@/lib/api/generated/zod.gen";
import {
  apiFieldErrors,
  describeResourceError,
  isUnavailable,
  preflightOf,
} from "@/lib/resources/errors";
import { invalidateRepositories } from "@/lib/resources/queries";
import {
  buildRepositoryWrite,
  newRepositoryFormValues,
  type RepositoryFormValues,
  repositoryFormSchema,
  repositoryKindLabel,
  repositoryToFormValues,
  tlsModeChoiceLabel,
  tlsModeLabel,
} from "@/lib/resources/repository";
import { type FieldErrors, fieldErrors } from "@/lib/schemas";

const fields = [
  "name",
  "kind",
  "tls_mode",
  "root",
  "host",
  "port",
  "base_path",
  "username",
  "password",
  "ca_pem",
] as const;

const tlsChoices = ["plain", "explicit", "implicit"] as const;

export function RepositoryForm({ repository }: { repository?: Repository }) {
  const creating = repository === undefined;
  const router = useRouter();
  const queryClient = useQueryClient();
  const [values, setValues] = useState<RepositoryFormValues>(() =>
    repository === undefined
      ? newRepositoryFormValues()
      : repositoryToFormValues(repository),
  );
  const [errors, setErrors] = useState<FieldErrors<RepositoryFormValues>>({});
  const [formError, setFormError] = useState<unknown>(null);

  const create = useMutation(createRepositoryMutation());
  const update = useMutation(updateRepositoryMutation());
  const mutation = creating ? create : update;

  function updateValue<K extends keyof RepositoryFormValues>(
    key: K,
    value: RepositoryFormValues[K],
  ) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function changeKind(kind: RepositoryKind) {
    setValues((current) => ({
      ...current,
      kind,
      tls_mode: kind === "local" ? "none" : "",
    }));
  }

  function changeTlsMode(mode: TlsMode) {
    setValues((current) => ({
      ...current,
      tls_mode: mode,
      ca_pem: mode === "plain" ? "" : current.ca_pem,
    }));
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = repositoryFormSchema(creating).safeParse(values);
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      setFormError(null);
      return;
    }
    setErrors({});
    setFormError(null);
    const body = zRepositoryWriteWritable.safeParse(
      buildRepositoryWrite(values),
    );
    if (!body.success) {
      setErrors(fieldErrors(body.error));
      return;
    }
    const onSuccess = (saved: Repository) => {
      invalidateRepositories(queryClient, saved.id);
      toast.add({
        type: "success",
        title: creating ? "仓库已创建" : "仓库已更新",
        description: saved.name,
      });
      router.push(`/repositories/detail?id=${saved.id}`);
    };
    const onError = (error: unknown) => {
      const mapped = apiFieldErrors(error, fields);
      if (Object.keys(mapped).length > 0) {
        setErrors(mapped);
        return;
      }
      if (preflightOf(error) !== null) {
        return;
      }
      setFormError(error);
    };
    if (repository === undefined) {
      create.mutate({ body: body.data }, { onSuccess, onError });
      return;
    }
    update.mutate(
      { path: { id: repository.id }, body: body.data },
      { onSuccess, onError },
    );
  }

  const preflight = preflightOf(mutation.error);
  const isFtp = values.kind === "ftp";
  const passwordPlaceholder =
    repository === undefined
      ? "请输入 FTP 密码"
      : repository.has_secret
        ? "已设置（留空保持不变）"
        : "未设置";

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

      <FieldGroup>
        <Field data-invalid={errors.name ? true : undefined}>
          <FieldLabel htmlFor="repository-name">名称</FieldLabel>
          <Input
            id="repository-name"
            value={values.name}
            onChange={(event) => updateValue("name", event.target.value)}
            aria-invalid={errors.name ? true : undefined}
            autoComplete="off"
          />
          <FieldDescription>1–64 个字符。</FieldDescription>
          <FieldError>{errors.name}</FieldError>
        </Field>

        <FieldSet>
          <FieldLegend>仓库类型</FieldLegend>
          {repository !== undefined ? (
            <Field>
              <p className="text-sm font-medium">类型与传输方式</p>
              <p className="text-sm">
                {repositoryKindLabel[repository.kind]} ·{" "}
                {tlsModeLabel[repository.tls_mode]}
              </p>
              <FieldDescription>
                类型与传输方式在创建后不可更改；如需变更请新建仓库。
              </FieldDescription>
            </Field>
          ) : (
            <RadioGroup
              aria-label="仓库类型"
              value={values.kind}
              onValueChange={(value) => changeKind(value as RepositoryKind)}
            >
              <div className="flex items-center gap-2 text-sm">
                <RadioGroupItem value="local" aria-label="本地目录" />
                <span>本地目录</span>
              </div>
              <div className="flex items-center gap-2 text-sm">
                <RadioGroupItem value="ftp" aria-label="FTP / FTPS" />
                <span>FTP / FTPS</span>
              </div>
            </RadioGroup>
          )}
        </FieldSet>

        {values.kind === "local" ? (
          <Field data-invalid={errors.root ? true : undefined}>
            <FieldLabel htmlFor="repository-root">根目录</FieldLabel>
            <Input
              id="repository-root"
              value={values.root}
              onChange={(event) => updateValue("root", event.target.value)}
              aria-invalid={errors.root ? true : undefined}
              placeholder="/backup/repository"
              className="font-mono"
              autoComplete="off"
            />
            <FieldDescription>
              intern 容器内可见的绝对路径；不能包含 KEK 或工作根目录。
            </FieldDescription>
            <FieldError>{errors.root}</FieldError>
          </Field>
        ) : null}

        {isFtp ? (
          <>
            <FieldSet>
              <FieldLegend>传输方式</FieldLegend>
              {repository !== undefined ? (
                <p className="text-sm">
                  {tlsModeLabel[repository.tls_mode]}（创建后不可更改）
                </p>
              ) : (
                <>
                  <RadioGroup
                    aria-label="FTP 传输方式"
                    value={values.tls_mode === "" ? null : values.tls_mode}
                    onValueChange={(value) => changeTlsMode(value as TlsMode)}
                    aria-invalid={errors.tls_mode ? true : undefined}
                  >
                    {tlsChoices.map((mode) => (
                      <div key={mode} className="flex flex-col gap-1 text-sm">
                        <span className="flex items-center gap-2">
                          <RadioGroupItem
                            value={mode}
                            aria-label={tlsModeChoiceLabel[mode].title}
                          />
                          {tlsModeChoiceLabel[mode].title}
                        </span>
                        <span className="pl-6 text-muted-foreground">
                          {tlsModeChoiceLabel[mode].description}
                        </span>
                      </div>
                    ))}
                  </RadioGroup>
                  <FieldDescription>
                    必须显式选择，不会根据地址或端口猜测；省略传输方式的请求不会发出。
                  </FieldDescription>
                  <FieldError>{errors.tls_mode}</FieldError>
                </>
              )}
            </FieldSet>

            {values.tls_mode === "plain" ? <PlaintextWarning /> : null}

            <div className="grid gap-4 sm:grid-cols-2">
              <Field data-invalid={errors.host ? true : undefined}>
                <FieldLabel htmlFor="repository-host">主机</FieldLabel>
                <Input
                  id="repository-host"
                  value={values.host}
                  onChange={(event) => updateValue("host", event.target.value)}
                  aria-invalid={errors.host ? true : undefined}
                  autoComplete="off"
                />
                <FieldError>{errors.host}</FieldError>
              </Field>
              <Field data-invalid={errors.port ? true : undefined}>
                <FieldLabel htmlFor="repository-port">端口</FieldLabel>
                <Input
                  id="repository-port"
                  type="number"
                  min={1}
                  max={65535}
                  value={values.port}
                  onChange={(event) => updateValue("port", event.target.value)}
                  aria-invalid={errors.port ? true : undefined}
                />
                <FieldError>{errors.port}</FieldError>
              </Field>
            </div>

            <Field data-invalid={errors.base_path ? true : undefined}>
              <FieldLabel htmlFor="repository-base-path">基础路径</FieldLabel>
              <Input
                id="repository-base-path"
                value={values.base_path}
                onChange={(event) =>
                  updateValue("base_path", event.target.value)
                }
                aria-invalid={errors.base_path ? true : undefined}
                placeholder="/backups"
                className="font-mono"
                autoComplete="off"
              />
              <FieldError>{errors.base_path}</FieldError>
            </Field>

            <Field data-invalid={errors.username ? true : undefined}>
              <FieldLabel htmlFor="repository-username">用户名</FieldLabel>
              <Input
                id="repository-username"
                value={values.username}
                onChange={(event) =>
                  updateValue("username", event.target.value)
                }
                aria-invalid={errors.username ? true : undefined}
                autoComplete="off"
              />
              <FieldError>{errors.username}</FieldError>
            </Field>

            <Field data-invalid={errors.password ? true : undefined}>
              <FieldLabel htmlFor="repository-password">密码</FieldLabel>
              <Input
                id="repository-password"
                type="password"
                value={values.password}
                onChange={(event) =>
                  updateValue("password", event.target.value)
                }
                aria-invalid={errors.password ? true : undefined}
                placeholder={passwordPlaceholder}
                autoComplete="new-password"
              />
              <FieldDescription>
                密码只写不读，服务端不会返回已保存的值；编辑时留空表示保留现有密码。
              </FieldDescription>
              <FieldError>{errors.password}</FieldError>
            </Field>

            {values.tls_mode === "explicit" ||
            values.tls_mode === "implicit" ? (
              <Field data-invalid={errors.ca_pem ? true : undefined}>
                <FieldLabel htmlFor="repository-ca-pem">CA bundle</FieldLabel>
                <Textarea
                  id="repository-ca-pem"
                  value={values.ca_pem}
                  onChange={(event) =>
                    updateValue("ca_pem", event.target.value)
                  }
                  aria-invalid={errors.ca_pem ? true : undefined}
                  rows={6}
                  className="font-mono"
                  placeholder="-----BEGIN CERTIFICATE-----"
                />
                <FieldDescription>
                  可选。填写签发服务器证书的公共 PEM
                  证书链；留空使用系统根证书。不会跳过证书校验。
                </FieldDescription>
                <FieldError>{errors.ca_pem}</FieldError>
              </Field>
            ) : null}
          </>
        ) : null}
      </FieldGroup>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner data-icon="inline-start" /> : null}
          {creating ? "创建仓库" : "保存修改"}
        </Button>
        <Button
          nativeButton={false}
          variant="outline"
          render={
            <Link
              href={
                repository !== undefined
                  ? `/repositories/detail?id=${repository.id}`
                  : "/repositories"
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
