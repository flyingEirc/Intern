import type { Metadata } from "next";
import { AuditView } from "./audit-view";

export const metadata: Metadata = { title: "审计日志" };

export default function AuditPage() {
  return <AuditView />;
}
