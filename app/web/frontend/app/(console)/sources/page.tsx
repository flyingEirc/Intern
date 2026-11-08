import type { Metadata } from "next";
import { SourcesView } from "./sources-view";

export const metadata: Metadata = { title: "MySQL 数据源" };

export default function SourcesPage() {
  return <SourcesView />;
}
