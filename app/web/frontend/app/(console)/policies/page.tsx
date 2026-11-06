import type { Metadata } from "next";
import { PoliciesView } from "./policies-view";

export const metadata: Metadata = { title: "备份策略" };

export default function PoliciesPage() {
  return <PoliciesView />;
}
