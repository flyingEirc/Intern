"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon, InfoIcon } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";
import { type FormEvent, useEffect, useState } from "react";
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
  FieldError,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { isUnauthorized } from "@/lib/api/errors";
import {
  currentSessionOptions,
  getSetupOptions,
  loginMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import { describeError } from "@/lib/api/messages";
import type { LoginReason } from "@/lib/navigation";
import { safeNextPath } from "@/lib/navigation";
import { forgetPrincipal } from "@/lib/principal";
import { type FieldErrors, fieldErrors, loginFormSchema } from "@/lib/schemas";

const reasonText: Record<LoginReason, string> = {
  expired: "会话已失效，请重新登录。",
  "password-changed": "密码已修改，请使用新密码重新登录。",
  "signed-out": "已退出登录。",
};

type LoginFields = { username: string; password: string };

export function LoginForm() {
  const router = useRouter();
  const params = useSearchParams();
  const queryClient = useQueryClient();
  const next = safeNextPath(params.get("next"));
  const reason = params.get("reason") as LoginReason | null;
  const [errors, setErrors] = useState<FieldErrors<LoginFields>>({});

  // Someone who still has a valid session goes straight to the destination.
  const session = useQuery({
    ...currentSessionOptions(),
    retry: false,
    meta: { handlesUnauthorized: true },
  });
  useEffect(() => {
    if (session.data) {
      router.replace(next);
    }
  }, [session.data, router, next]);
  const setup = useQuery({
    ...getSetupOptions(),
    retry: false,
    meta: { handlesUnauthorized: true },
  });
  useEffect(() => {
    if (setup.data?.required) {
      router.replace("/setup");
    }
  }, [setup.data, router]);

  const login = useMutation({
    ...loginMutation(),
    meta: { handlesUnauthorized: true },
    onSuccess: async () => {
      // The previous principal's cache must not leak into the new session.
      forgetPrincipal(queryClient);
      router.replace(next);
    },
  });

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const parsed = loginFormSchema.safeParse({
      username: form.get("username"),
      password: form.get("password"),
    });
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      return;
    }
    setErrors({});
    login.mutate({ body: parsed.data });
  }

  const failure = login.error
    ? isUnauthorized(login.error)
      ? "用户名或密码错误。"
      : describeError(login.error)
    : null;

  return (
    <Card>
      <CardHeader>
        <CardTitle>登录</CardTitle>
        <CardDescription>使用 Intern 账号登录控制台</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {reason && reason in reasonText && !failure ? (
          <Alert>
            <InfoIcon />
            <AlertDescription>{reasonText[reason]}</AlertDescription>
          </Alert>
        ) : null}
        {failure ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>登录失败</AlertTitle>
            <AlertDescription>{failure}</AlertDescription>
          </Alert>
        ) : null}
        <form noValidate onSubmit={onSubmit}>
          <FieldGroup>
            <Field data-invalid={errors.username ? true : undefined}>
              <FieldLabel htmlFor="username">用户名</FieldLabel>
              <Input
                id="username"
                name="username"
                autoComplete="username"
                autoFocus
                aria-invalid={errors.username ? true : undefined}
              />
              <FieldError>{errors.username}</FieldError>
            </Field>
            <Field data-invalid={errors.password ? true : undefined}>
              <FieldLabel htmlFor="password">密码</FieldLabel>
              <Input
                id="password"
                name="password"
                type="password"
                autoComplete="current-password"
                aria-invalid={errors.password ? true : undefined}
              />
              <FieldError>{errors.password}</FieldError>
            </Field>
            <Field>
              <Button type="submit" disabled={login.isPending}>
                {login.isPending ? <Spinner data-icon="inline-start" /> : null}
                登录
              </Button>
            </Field>
          </FieldGroup>
        </form>
      </CardContent>
    </Card>
  );
}
