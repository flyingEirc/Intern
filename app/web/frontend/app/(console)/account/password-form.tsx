"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon } from "lucide-react";
import { useRouter } from "next/navigation";
import { type FormEvent, useRef, useState } from "react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
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
import { toast } from "@/components/ui/toast";
import { ApiError, isUnauthorized } from "@/lib/api/errors";
import {
  changeOwnPasswordMutation,
  currentSessionOptions,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import { describeError } from "@/lib/api/messages";
import { loginPath } from "@/lib/navigation";
import { forgetPrincipal } from "@/lib/principal";
import {
  type FieldErrors,
  fieldErrors,
  passwordFormSchema,
} from "@/lib/schemas";

type PasswordFields = {
  current_password: string;
  new_password: string;
  confirm_password: string;
};

export function PasswordForm() {
  const router = useRouter();
  const queryClient = useQueryClient();
  const formRef = useRef<HTMLFormElement>(null);
  const [errors, setErrors] = useState<FieldErrors<PasswordFields>>({});

  const change = useMutation({
    ...changeOwnPasswordMutation(),
    onSuccess: async () => {
      formRef.current?.reset();
      // A 204 can also mean the password changed but token rotation failed,
      // in which case this session is no longer valid (see the contract).
      try {
        await queryClient.fetchQuery({
          ...currentSessionOptions(),
          staleTime: 0,
          retry: false,
          meta: { handlesUnauthorized: true },
        });
      } catch (error) {
        if (isUnauthorized(error)) {
          forgetPrincipal(queryClient);
          router.replace(loginPath("/", "password-changed"));
          return;
        }
      }
      toast.add({
        type: "success",
        title: "密码已修改",
        description: "本会话保持登录，其他会话已全部失效。",
      });
    },
    onError: (error) => {
      // The contract answers a wrong current password with 400; the form has
      // already enforced the only new-password rule (12–256 characters).
      if (error instanceof ApiError && error.status === 400) {
        setErrors({ current_password: "当前密码不正确" });
      }
    },
  });

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const form = new FormData(event.currentTarget);
    const parsed = passwordFormSchema.safeParse({
      current_password: form.get("current_password"),
      new_password: form.get("new_password"),
      confirm_password: form.get("confirm_password"),
    });
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      return;
    }
    setErrors({});
    change.mutate({
      body: {
        current_password: parsed.data.current_password,
        new_password: parsed.data.new_password,
      },
    });
  }

  const failure =
    change.error &&
    !(change.error instanceof ApiError && change.error.status === 400)
      ? describeError(change.error)
      : null;

  return (
    <Card className="max-w-lg">
      <form ref={formRef} noValidate onSubmit={onSubmit}>
        <CardHeader>
          <CardTitle>修改密码</CardTitle>
          <CardDescription>
            修改后本会话继续有效，本账号的其他会话会被全部吊销。
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4 py-6">
          {failure ? (
            <Alert variant="destructive">
              <CircleAlertIcon />
              <AlertTitle>修改失败</AlertTitle>
              <AlertDescription>{failure}</AlertDescription>
            </Alert>
          ) : null}
          <FieldGroup>
            <Field data-invalid={errors.current_password ? true : undefined}>
              <FieldLabel htmlFor="current_password">当前密码</FieldLabel>
              <Input
                id="current_password"
                name="current_password"
                type="password"
                autoComplete="current-password"
                aria-invalid={errors.current_password ? true : undefined}
              />
              <FieldError>{errors.current_password}</FieldError>
            </Field>
            <Field data-invalid={errors.new_password ? true : undefined}>
              <FieldLabel htmlFor="new_password">新密码</FieldLabel>
              <Input
                id="new_password"
                name="new_password"
                type="password"
                autoComplete="new-password"
                aria-invalid={errors.new_password ? true : undefined}
              />
              <FieldDescription>12–256 个字符。</FieldDescription>
              <FieldError>{errors.new_password}</FieldError>
            </Field>
            <Field data-invalid={errors.confirm_password ? true : undefined}>
              <FieldLabel htmlFor="confirm_password">确认新密码</FieldLabel>
              <Input
                id="confirm_password"
                name="confirm_password"
                type="password"
                autoComplete="new-password"
                aria-invalid={errors.confirm_password ? true : undefined}
              />
              <FieldError>{errors.confirm_password}</FieldError>
            </Field>
          </FieldGroup>
        </CardContent>
        <CardFooter>
          <Button type="submit" disabled={change.isPending}>
            {change.isPending ? <Spinner data-icon="inline-start" /> : null}
            修改密码
          </Button>
        </CardFooter>
      </form>
    </Card>
  );
}
