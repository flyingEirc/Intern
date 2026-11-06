"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
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
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { toast } from "@/components/ui/toast";
import {
  createSourceMutation,
  updateSourceMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import type { Source } from "@/lib/api/generated/types.gen";
import { zSourceWriteWritable } from "@/lib/api/generated/zod.gen";
import {
  apiFieldErrors,
  describeResourceError,
  isUnavailable,
  preflightOf,
} from "@/lib/resources/errors";
import { invalidateSources } from "@/lib/resources/queries";
import {
  buildSourceWrite,
  newSourceFormValues,
  type SourceFormValues,
  sourceFormSchema,
  sourceToFormValues,
} from "@/lib/resources/source";
import { type FieldErrors, fieldErrors } from "@/lib/schemas";

const fields = ["name", "socket", "datadir", "username", "password"] as const;

export function SourceForm({ source }: { source?: Source }) {
  const creating = source === undefined;
  const router = useRouter();
  const queryClient = useQueryClient();
  const [values, setValues] = useState<SourceFormValues>(() =>
    source === undefined ? newSourceFormValues() : sourceToFormValues(source),
  );
  const [errors, setErrors] = useState<FieldErrors<SourceFormValues>>({});
  const [formError, setFormError] = useState<unknown>(null);

  const create = useMutation(createSourceMutation());
  const update = useMutation(updateSourceMutation());
  const mutation = creating ? create : update;

  function updateValue<K extends keyof SourceFormValues>(
    key: K,
    value: SourceFormValues[K],
  ) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = sourceFormSchema(creating).safeParse(values);
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      setFormError(null);
      return;
    }
    setErrors({});
    setFormError(null);
    const body = zSourceWriteWritable.safeParse(buildSourceWrite(values));
    if (!body.success) {
      setErrors(fieldErrors(body.error));
      return;
    }
    const onSuccess = (saved: Source) => {
      invalidateSources(queryClient, saved.id);
      toast.add({
        type: "success",
        title: creating ? "数据源已创建" : "数据源已更新",
        description: saved.name,
      });
      router.push(`/sources/detail?id=${saved.id}`);
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
    if (source === undefined) {
      create.mutate({ body: body.data }, { onSuccess, onError });
      return;
    }
    update.mutate(
      { path: { id: source.id }, body: body.data },
      { onSuccess, onError },
    );
  }

  const preflight = preflightOf(mutation.error);
  const passwordPlaceholder =
    source === undefined
      ? "请输入 MySQL 密码"
      : source.has_secret
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
          <FieldLabel htmlFor="source-name">名称</FieldLabel>
          <Input
            id="source-name"
            value={values.name}
            onChange={(event) => updateValue("name", event.target.value)}
            aria-invalid={errors.name ? true : undefined}
            autoComplete="off"
          />
          <FieldDescription>1–64 个字符。</FieldDescription>
          <FieldError>{errors.name}</FieldError>
        </Field>

        <Field data-invalid={errors.socket ? true : undefined}>
          <FieldLabel htmlFor="source-socket">Socket</FieldLabel>
          <Input
            id="source-socket"
            value={values.socket}
            onChange={(event) => updateValue("socket", event.target.value)}
            aria-invalid={errors.socket ? true : undefined}
            placeholder="/run/mysqld/mysqld.sock"
            className="font-mono"
            autoComplete="off"
          />
          <FieldDescription>
            intern 容器内可见的 MySQL unix socket 绝对路径。
          </FieldDescription>
          <FieldError>{errors.socket}</FieldError>
        </Field>

        <Field data-invalid={errors.datadir ? true : undefined}>
          <FieldLabel htmlFor="source-datadir">Datadir</FieldLabel>
          <Input
            id="source-datadir"
            value={values.datadir}
            onChange={(event) => updateValue("datadir", event.target.value)}
            aria-invalid={errors.datadir ? true : undefined}
            placeholder="/var/lib/mysql"
            className="font-mono"
            autoComplete="off"
          />
          <FieldDescription>
            只读挂载的 MySQL 数据目录绝对路径。
          </FieldDescription>
          <FieldError>{errors.datadir}</FieldError>
        </Field>

        <Field data-invalid={errors.username ? true : undefined}>
          <FieldLabel htmlFor="source-username">用户名</FieldLabel>
          <Input
            id="source-username"
            value={values.username}
            onChange={(event) => updateValue("username", event.target.value)}
            aria-invalid={errors.username ? true : undefined}
            autoComplete="off"
          />
          <FieldError>{errors.username}</FieldError>
        </Field>

        <Field data-invalid={errors.password ? true : undefined}>
          <FieldLabel htmlFor="source-password">密码</FieldLabel>
          <Input
            id="source-password"
            type="password"
            value={values.password}
            onChange={(event) => updateValue("password", event.target.value)}
            aria-invalid={errors.password ? true : undefined}
            placeholder={passwordPlaceholder}
            autoComplete="new-password"
          />
          <FieldDescription>
            密码只写不读，服务端不会返回已保存的值；编辑时留空表示保留现有密码。
          </FieldDescription>
          <FieldError>{errors.password}</FieldError>
        </Field>
      </FieldGroup>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? <Spinner data-icon="inline-start" /> : null}
          {creating ? "创建数据源" : "保存修改"}
        </Button>
        <Button
          nativeButton={false}
          variant="outline"
          render={
            <Link
              href={
                source !== undefined
                  ? `/sources/detail?id=${source.id}`
                  : "/sources"
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
