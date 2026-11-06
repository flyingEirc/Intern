"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { CircleAlertIcon, ShieldOffIcon } from "lucide-react";
import { useRouter, useSearchParams } from "next/navigation";
import { type FormEvent, useState } from "react";
import {
  DetailSkeleton,
  NoPermission,
  PageHeader,
  ResourceError,
} from "@/components/resources/states";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
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
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  Field,
  FieldDescription,
  FieldError,
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
import { ApiError, isUnauthorized } from "@/lib/api/errors";
import {
  revokeUserSessionsMutation,
  updateUserMutation,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import type { Role, User } from "@/lib/api/generated/types.gen";
import { describeError } from "@/lib/api/messages";
import { loginPath } from "@/lib/navigation";
import { forgetPrincipal } from "@/lib/principal";
import { formatDateTime } from "@/lib/resources/format";
import { hasPermission } from "@/lib/resources/permissions";
import { invalidateUsers, useUser } from "@/lib/resources/queries";
import {
  isLastAdminConflict,
  lastAdminExplanation,
  passwordResetSchema,
  roleSelectItems,
  roles,
} from "@/lib/resources/user";
import { roleLabel } from "@/lib/roles";

const sessionRevokeNotice =
  "修改角色、启用或禁用、重置密码都会吊销该用户的全部会话，对方需要重新登录。";

const selfSessionNotice =
  "这是你正在使用的账号。保存后当前会话会被吊销，你需要重新登录。";

function changeError(error: unknown): string {
  if (isLastAdminConflict(error)) {
    return lastAdminExplanation;
  }
  if (error instanceof ApiError && error.code === "conflict") {
    return "用户名已存在，未做任何修改。";
  }
  return describeError(error);
}

export function UserDetail() {
  const id = useSearchParams().get("id");
  const session = useSession();
  const canManage = hasPermission(session.data?.permissions, "users:manage");
  const account = useUser(canManage ? id : null);

  if (!canManage) {
    return (
      <NoPermission
        action="管理用户"
        note="当前账号没有管理用户的权限。用户详情只对管理员开放，服务端仍会逐请求拒绝。"
      />
    );
  }
  if (id === null) {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyTitle>缺少用户 ID</EmptyTitle>
          <EmptyDescription>
            请从用户列表打开详情；地址形如 /users/detail?id=…
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  if (account.isPending) {
    return <DetailSkeleton />;
  }
  if (account.isError) {
    return (
      <ResourceError
        title="无法读取用户"
        error={account.error}
        onRetry={() => account.refetch()}
      />
    );
  }

  return (
    <UserEditor
      key={`${account.data.id}:${account.data.updated_at}`}
      account={account.data}
      self={session.data?.user.id === account.data.id}
    />
  );
}

function UserEditor({ account, self }: { account: User; self: boolean }) {
  const router = useRouter();
  const queryClient = useQueryClient();
  const [role, setRole] = useState<Role>(account.role);
  const [disabled, setDisabled] = useState(account.disabled);
  const [password, setPassword] = useState("");
  const [passwordError, setPasswordError] = useState<string | undefined>();
  const [saveError, setSaveError] = useState<unknown>(null);
  const [revokeOpen, setRevokeOpen] = useState(false);
  const [revokeError, setRevokeError] = useState<unknown>(null);
  const update = useMutation(updateUserMutation());
  const revoke = useMutation(revokeUserSessionsMutation());

  const roleChanged = role !== account.role;
  const disabledChanged = disabled !== account.disabled;
  const resetting = password !== "";
  const dirty = roleChanged || disabledChanged || resetting;

  function saved(updated: User) {
    setPassword("");
    setPasswordError(undefined);
    invalidateUsers(queryClient, updated.id);
    setSaveError(null);
    toast.add({
      type: "success",
      title: "用户已更新",
      description: "该用户的全部会话已被吊销。",
    });
    if (self) {
      forgetPrincipal(queryClient);
      router.replace(loginPath("/users", "expired"));
    }
  }

  function onSave(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = passwordResetSchema.safeParse({ password });
    if (!parsed.success) {
      setPasswordError(parsed.error.issues[0]?.message);
      return;
    }
    setPasswordError(undefined);
    if (!dirty) {
      return;
    }
    setSaveError(null);
    update.mutate(
      {
        path: { id: account.id },
        body: {
          ...(roleChanged ? { role } : {}),
          ...(disabledChanged ? { disabled } : {}),
          ...(resetting ? { password: parsed.data.password } : {}),
        },
      },
      {
        onSuccess: saved,
        onError: (error) => {
          if (self && isUnauthorized(error)) {
            forgetPrincipal(queryClient);
            router.replace(loginPath("/users", "expired"));
            return;
          }
          setSaveError(error);
        },
      },
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <PageHeader
        title={account.username}
        description="角色、启用状态与密码。密码只写，保存后输入框会清空。"
      />
      <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted-foreground">用户名</dt>
        <dd className="font-medium">{account.username}</dd>
        <dt className="text-muted-foreground">当前角色</dt>
        <dd>
          <Badge variant="outline">{roleLabel[account.role]}</Badge>
        </dd>
        <dt className="text-muted-foreground">当前状态</dt>
        <dd>
          <Badge variant={account.disabled ? "destructive" : "secondary"}>
            {account.disabled ? "已禁用" : "已启用"}
          </Badge>
        </dd>
        <dt className="text-muted-foreground">创建时间</dt>
        <dd>{formatDateTime(account.created_at)}</dd>
        <dt className="text-muted-foreground">更新时间</dt>
        <dd>{formatDateTime(account.updated_at)}</dd>
      </dl>

      <Alert>
        <ShieldOffIcon />
        <AlertTitle>保存会吊销会话</AlertTitle>
        <AlertDescription>
          {sessionRevokeNotice}
          {self ? selfSessionNotice : null}
        </AlertDescription>
      </Alert>

      <form
        noValidate
        onSubmit={onSave}
        className="flex max-w-xl flex-col gap-4"
      >
        {saveError ? (
          <Alert variant="destructive">
            <CircleAlertIcon />
            <AlertTitle>
              {isLastAdminConflict(saveError)
                ? "无法修改最后一个管理员"
                : "保存失败"}
            </AlertTitle>
            <AlertDescription>{changeError(saveError)}</AlertDescription>
          </Alert>
        ) : null}
        <Field>
          <FieldLabel htmlFor="user-role">角色</FieldLabel>
          <Select
            items={roleSelectItems}
            value={role}
            onValueChange={(value) => setRole(value as Role)}
          >
            <SelectTrigger id="user-role" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                {roles.map((item) => (
                  <SelectItem key={item} value={item}>
                    {roleSelectItems[item]}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
        <Field>
          <FieldLabel htmlFor="user-disabled">启用状态</FieldLabel>
          <Select
            items={{ enabled: "已启用", disabled: "已禁用" }}
            value={disabled ? "disabled" : "enabled"}
            onValueChange={(value) => setDisabled(value === "disabled")}
          >
            <SelectTrigger id="user-disabled" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="enabled">已启用</SelectItem>
                <SelectItem value="disabled">已禁用</SelectItem>
              </SelectGroup>
            </SelectContent>
          </Select>
          <FieldDescription>
            禁用后该用户无法登录，已有会话也会被吊销。
          </FieldDescription>
        </Field>
        <Field data-invalid={passwordError ? true : undefined}>
          <FieldLabel htmlFor="user-password">重置密码</FieldLabel>
          <Input
            id="user-password"
            type="password"
            value={password}
            autoComplete="new-password"
            aria-invalid={passwordError ? true : undefined}
            onChange={(event) => setPassword(event.target.value)}
          />
          <FieldDescription>
            留空表示不修改密码。填写则为 12–256
            个字符，提交后立即清空，页面不会保留或回显。
          </FieldDescription>
          <FieldError>{passwordError}</FieldError>
        </Field>
        <Button type="submit" disabled={!dirty || update.isPending}>
          {update.isPending ? <Spinner data-icon="inline-start" /> : null}
          保存修改
        </Button>
      </form>

      <Card className="max-w-xl">
        <CardHeader>
          <CardTitle>吊销全部会话</CardTitle>
          <CardDescription>
            不改变角色、状态或密码，只让该用户的已有登录全部失效。
            {self ? selfSessionNotice : null}
          </CardDescription>
        </CardHeader>
        <CardContent>
          {revokeError ? (
            <Alert variant="destructive">
              <CircleAlertIcon />
              <AlertTitle>吊销失败</AlertTitle>
              <AlertDescription>{describeError(revokeError)}</AlertDescription>
            </Alert>
          ) : null}
        </CardContent>
        <CardFooter>
          <AlertDialog open={revokeOpen} onOpenChange={setRevokeOpen}>
            <AlertDialogTrigger
              render={
                <Button variant="destructive" disabled={revoke.isPending} />
              }
            >
              {revoke.isPending ? <Spinner data-icon="inline-start" /> : null}
              吊销全部会话
            </AlertDialogTrigger>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>
                  吊销 {account.username} 的全部会话？
                </AlertDialogTitle>
                <AlertDialogDescription>
                  该用户已打开的页面会在下一次请求时退出到登录页。
                  {self
                    ? " 这包括你当前的会话，确认后需要重新登录。"
                    : " 此操作不修改密码。"}
                </AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={revoke.isPending}>
                  取消
                </AlertDialogCancel>
                <AlertDialogAction
                  variant="destructive"
                  disabled={revoke.isPending}
                  onClick={() => {
                    setRevokeError(null);
                    revoke.mutate(
                      { path: { id: account.id } },
                      {
                        onSuccess: () => {
                          setRevokeOpen(false);
                          invalidateUsers(queryClient, account.id);
                          if (self) {
                            forgetPrincipal(queryClient);
                            router.replace(loginPath("/users", "expired"));
                            return;
                          }
                          toast.add({
                            type: "success",
                            title: "会话已吊销",
                            description: account.username,
                          });
                        },
                        onError: (error) => {
                          setRevokeOpen(false);
                          if (self && isUnauthorized(error)) {
                            forgetPrincipal(queryClient);
                            router.replace(loginPath("/users", "expired"));
                            return;
                          }
                          setRevokeError(error);
                        },
                      },
                    );
                  }}
                >
                  {revoke.isPending ? (
                    <Spinner data-icon="inline-start" />
                  ) : null}
                  确认吊销
                </AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        </CardFooter>
      </Card>
    </div>
  );
}
