import type { Metadata } from "next";
import { RestoresView } from "./restores-view";

export const metadata: Metadata = { title: "恢复" };

export default function RestoresPage() {
  return <RestoresView />;
}
