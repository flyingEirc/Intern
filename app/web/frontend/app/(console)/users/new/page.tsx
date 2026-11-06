import type { Metadata } from "next";
import { UserForm } from "./user-form";

export const metadata: Metadata = { title: "新建用户" };

export default function NewUserPage() {
  return <UserForm />;
}
