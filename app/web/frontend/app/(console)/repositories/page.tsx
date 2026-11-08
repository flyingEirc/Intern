import type { Metadata } from "next";
import { RepositoriesView } from "./repositories-view";

export const metadata: Metadata = { title: "备份仓库" };

export default function RepositoriesPage() {
  return <RepositoriesView />;
}
