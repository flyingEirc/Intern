import type { Metadata } from "next";
import { PasswordForm } from "./password-form";

export const metadata: Metadata = { title: "账户与密码" };

export default function AccountPage() {
  return <PasswordForm />;
}
