import type { Metadata } from "next";
import { Overview } from "./overview";

export const metadata: Metadata = { title: "概览" };

export default function OverviewPage() {
  return <Overview />;
}
