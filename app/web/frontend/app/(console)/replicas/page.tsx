import type { Metadata } from "next";
import { ReplicasView } from "./replicas-view";

export const metadata: Metadata = { title: "异地副本" };

export default function ReplicasPage() {
  return <ReplicasView />;
}
