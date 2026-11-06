"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon } from "lucide-react";
import { useRouter } from "next/navigation";
import { type FormEvent, useState } from "react";
import {
  NoPermission,
  PageHeader,
  UnavailableAlert,
} from "@/components/resources/states";
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
import { toast } from "@/components/ui/toast";
import { useSession } from "@/hooks/use-session";
import { createUserMutation } from "@/lib/api/generated/@tanstack/react-query.gen";
import type { Role, User } from "@/lib/api/generated/types.gen";
import { describeError } from "@/lib/api/messages";
import { isUnavailable } from "@/lib/resources/errors";
import { hasPermission } from "@/lib/resources/permissions";
import { invalidateUsers } from "@/lib/resources/queries";
import {
  type NewUserFormValues,
  newUserFormSchema,
  newUserFormValues,
  roleSelectItems,
  roles,
} from "@/lib/resources/user";
import { type FieldErrors, fieldErrors } from "@/lib/schemas";

export function UserForm() {
  const session = useSession();
  const canManage = hasPermission(session.data?.permissions, "users:manage");
  const router = useRouter();
  const queryClient = useQueryClient();
  const [values, setValues] = useState<NewUserFormValues>(newUserFormValues);
  const [errors, setErrors] = useState<FieldErrors<NewUserFormValues>>({});
  const [formError, setFormError] = useState<unknown>(null);
  const create = useMutation(createUserMutation());

  if (!canManage) {
    return (
      <NoPermission
        action="创建用户"
        note="当前账号没有创建用户的权限。只有管理员可以新建账号。"
      />
    );
  }

  function updateValue<K extends keyof NewUserFormValues>(
    key: K,
    value: NewUserFormValues[K],
  ) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = newUserFormSchema.safeParse(values);
    if (!parsed.success) {
      setErrors(fieldErrors(parsed.error));
      setFormError(null);
      return;
    }
    setErrors({});
    setFormError(null);
    create.mutate(
      { body: parsed.data },
      {
        onSuccess: (created: User) => {
          invalidateUsers(queryClient, created.id);
          setValues(newUserFormValues());
          toast.add({
            type: "success",
            title: "用户已创建",
            description: created.username,
          });
          router.push(`/users/detail?id=${created.id}`);
        },
        onError: (error) => setFormError(error),
      },
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title="新建用户"
        description="初始密码只在这里输入一次，创建后不再显示。对方用它登录。"
      />
      <form
        noValidate
        onSubmit={onSubmit}
        className="flex max-w-xl flex-col gap-6"
      >
        {formError ? (
          isUnavailable(formError) ? (
            <UnavailableAlert error={formError} />
          ) : (
            <Alert variant="destructive">
              <CircleAlertIcon />
              <AlertTitle>创建失败</AlertTitle>
              <AlertDescription>{describeError(formError)}</AlertDescription>
            </Alert>
          )
        ) : null}
        <FieldGroup>
          <Field data-invalid={errors.username ? true : undefined}>
            <FieldLabel htmlFor="user-username">用户名</FieldLabel>
            <Input
              id="user-username"
              value={values.username}
              autoComplete="off"
              aria-invalid={errors.username ? true : undefined}
              onChange={(event) => updateValue("username", event.target.value)}
            />
            <FieldDescription>
              3–64 位，只能包含字母、数字、点、下划线和连字符。
            </FieldDescription>
            <FieldError>{errors.username}</FieldError>
          </Field>
          <Field data-invalid={errors.password ? true : undefined}>
            <FieldLabel htmlFor="user-password">初始密码</FieldLabel>
            <Input
              id="user-password"
              type="password"
              value={values.password}
              autoComplete="new-password"
              aria-invalid={errors.password ? true : undefined}
              onChange={(event) => updateValue("password", event.target.value)}
            />
            <FieldDescription>
              12–256 个字符。密码只写入，不会在任何页面回显。
            </FieldDescription>
            <FieldError>{errors.password}</FieldError>
          </Field>
          <Field data-invalid={errors.role ? true : undefined}>
            <FieldLabel htmlFor="user-role">角色</FieldLabel>
            <Select
              items={roleSelectItems}
              value={values.role}
              onValueChange={(value) => updateValue("role", value as Role)}
            >
              <SelectTrigger id="user-role" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  {roles.map((role) => (
                    <SelectItem key={role} value={role}>
                      {roleSelectItems[role]}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
            <FieldError>{errors.role}</FieldError>
          </Field>
          <Field>
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? <Spinner data-icon="inline-start" /> : null}
              创建用户
            </Button>
          </Field>
        </FieldGroup>
      </form>
    </div>
  );
}
