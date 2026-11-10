import type { Metadata } from "next";
import { SourcesView } from "./sources-view";

export const metadata: Metadata = { title: "数据源" };

export default function SourcesPage() {
  return <SourcesView />;
}
