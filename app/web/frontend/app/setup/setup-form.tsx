"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon, ShieldCheckIcon } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useRef, useState } from "react";
import { z } from "zod";
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
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { ApiError } from "@/lib/api/errors";
import {
  createSetupMutation,
  getSetupOptions,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import { zUsername } from "@/lib/api/generated/zod.gen";
import { describeError } from "@/lib/api/messages";
import { forgetPrincipal } from "@/lib/principal";
import { type FieldErrors, fieldErrors } from "@/lib/schemas";

const codePoints = (value: string) => [...value].length;

const setupSchema = z
  .object({
    username: z
      .string()
      .trim()
      .min(1, "请输入管理员用户名")
      .refine(
        (value) => zUsername.safeParse(value).success,
        "用户名只能包含字母、数字、点、下划线和连字符（3–64 位）",
      ),
    password: z
      .string()
      .refine(
        (value) => codePoints(value) >= 12 && codePoints(value) <= 256,
        "密码需为 12–256 个字符",
      ),
    confirm_password: z.string(),
  })
  .refine((value) => value.password === value.confirm_password, {
    path: ["confirm_password"],
    message: "两次输入的密码不一致",
  });

type SetupFields = z.infer<typeof setupSchema>;

export function SetupForm() {
  const router = useRouter();
  const queryClient = useQueryClient();
  const formRef = useRef<HTMLFormElement>(null);
  const [errors, setErrors] = useState<FieldErrors<SetupFields>>({});
  const status = useQuery({
    ...getSetupOptions(),
    retry: false,
    meta: { handlesUnauthorized: true },
  });
  const create = useMutation({
    ...createSetupMutation(),
    meta: { handlesUnauthorized: true },
    onSuccess: () => {
      formRef.current?.reset();
      forgetPrincipal(queryClient);
      router.replace("/");
    },
    onError: (error) => {
      if (error instanceof ApiError && error.code === "setup_completed") {
        formRef.current?.reset();
        void status.refetch();
      }
    },
  });

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const parsed = setupSchema.safeParse({
      username: form.get("username"),
      password: form.get("password"),
      confirm_password: form.get("confirm_password"),
    });
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      return;
    }
    setErrors({});
    create.mutate({
      body: {
        username: parsed.data.username,
        password: parsed.data.password,
      },
    });
  }

  const failure =
    create.error instanceof ApiError && create.error.code === "setup_completed"
      ? "初始化已经完成，请直接登录。"
      : create.error
        ? describeError(create.error)
        : null;

  if (status.isPending) {
    return (
      <Card aria-busy="true">
        <CardHeader>
          <CardTitle>正在检查初始化状态</CardTitle>
          <CardDescription>请稍候。</CardDescription>
        </CardHeader>
        <CardContent>
          <Spinner />
        </CardContent>
      </Card>
    );
  }

  if (status.isError) {
    return (
      <Alert variant="destructive">
        <CircleAlertIcon />
        <AlertTitle>无法读取初始化状态</AlertTitle>
        <AlertDescription className="flex flex-col items-start gap-3">
          {describeError(status.error)}
          <Button variant="outline" size="sm" onClick={() => status.refetch()}>
            重试
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!status.data.required) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>初始化已完成</CardTitle>
          <CardDescription>
            首位管理员已经创建，请使用账号登录。
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Button
            nativeButton={false}
            role="link"
            render={<Link href="/login" />}
          >
            前往登录
          </Button>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheckIcon />
          创建首位管理员
        </CardTitle>
        <CardDescription>创建管理员后即可开始配置数据库保护。</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {failure ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>初始化失败</AlertTitle>
            <AlertDescription>{failure}</AlertDescription>
          </Alert>
        ) : null}
        <form ref={formRef} noValidate onSubmit={onSubmit}>
          <FieldGroup>
            <Field data-invalid={errors.username ? true : undefined}>
              <FieldLabel htmlFor="setup-username">管理员用户名</FieldLabel>
              <Input
                id="setup-username"
                name="username"
                autoComplete="username"
                autoFocus
                aria-invalid={errors.username ? true : undefined}
              />
              <FieldError>{errors.username}</FieldError>
            </Field>
            <Field data-invalid={errors.password ? true : undefined}>
              <FieldLabel htmlFor="setup-password">设置密码</FieldLabel>
              <Input
                id="setup-password"
                name="password"
                type="password"
                autoComplete="new-password"
                aria-invalid={errors.password ? true : undefined}
              />
              <FieldDescription>12–256 个字符。</FieldDescription>
              <FieldError>{errors.password}</FieldError>
            </Field>
            <Field data-invalid={errors.confirm_password ? true : undefined}>
              <FieldLabel htmlFor="setup-confirm">确认密码</FieldLabel>
              <Input
                id="setup-confirm"
                name="confirm_password"
                type="password"
                autoComplete="new-password"
                aria-invalid={errors.confirm_password ? true : undefined}
              />
              <FieldError>{errors.confirm_password}</FieldError>
            </Field>
            <Field>
              <Button type="submit" disabled={create.isPending}>
                {create.isPending ? <Spinner data-icon="inline-start" /> : null}
                创建管理员并进入控制台
              </Button>
            </Field>
          </FieldGroup>
        </form>
      </CardContent>
    </Card>
  );
}
