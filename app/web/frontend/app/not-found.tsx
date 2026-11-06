import Link from "next/link";
import { buttonVariants } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";

export default function NotFound() {
  return (
    <main className="flex flex-1 items-center justify-center p-6">
      <Empty>
        <EmptyHeader>
          <EmptyTitle>页面不存在</EmptyTitle>
          <EmptyDescription>请检查地址，或返回控制台概览。</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Link href="/" className={buttonVariants({ variant: "outline" })}>
            返回概览
          </Link>
        </EmptyContent>
      </Empty>
    </main>
  );
}
