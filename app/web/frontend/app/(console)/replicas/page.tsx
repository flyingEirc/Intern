import type { Metadata } from "next";
import { ReplicasView } from "./replicas-view";

export const metadata: Metadata = { title: "副本复制" };

export default function ReplicasPage() {
  return <ReplicasView />;
}
