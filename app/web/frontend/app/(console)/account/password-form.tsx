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

// PUT /auth/password answers several cases with 400 validation_failed; only
// this fixed message (app/web/internal/server/auth_routes.go) means the current
// password was wrong. Any other 400 is shown as the backend reported it.
function isWrongCurrentPassword(error: unknown): boolean {
  return (
    error instanceof ApiError &&
    error.status === 400 &&
    error.code === "validation_failed" &&
    error.message === "current password is incorrect"
  );
}

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
        // The password did change; only this session's state is unknown.
        toast.add({
          type: "warning",
          title: "密码已修改",
          description: `无法确认本会话是否仍然有效：${describeError(error)}。如后续操作失败，请用新密码重新登录。`,
        });
        return;
      }
      toast.add({
        type: "success",
        title: "密码已修改",
        description: "本会话保持登录，其他会话已全部失效。",
      });
    },
    onError: (error) => {
      if (isWrongCurrentPassword(error)) {
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
    change.error && !isWrongCurrentPassword(change.error)
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
