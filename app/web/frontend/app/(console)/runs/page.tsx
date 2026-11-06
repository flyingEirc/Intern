import type { Metadata } from "next";
import { RunsView } from "./runs-view";

export const metadata: Metadata = { title: "运行记录" };

export default function RunsPage() {
  return <RunsView />;
}
