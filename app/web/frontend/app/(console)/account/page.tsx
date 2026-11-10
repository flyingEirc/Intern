import type { Metadata } from "next";
import { AccountProfile } from "./account-profile";
import { PasswordForm } from "./password-form";

export const metadata: Metadata = { title: "账户与密码" };

export default function AccountPage() {
  return (
    <div className="flex w-full max-w-4xl flex-col gap-5">
      <AccountProfile />
      <PasswordForm />
    </div>
  );
}
