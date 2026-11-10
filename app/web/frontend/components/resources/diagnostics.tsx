"use client";

import { ChevronRightIcon, ChevronUpIcon, InfoIcon } from "lucide-react";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";

/** Low-level recovery and engine details stay one deliberate interaction away. */
export function Diagnostics({
  title = "恢复诊断",
  description = "排查或联系支持时再展开技术细节；这些信息不是日常操作项。",
  children,
}: {
  title?: string;
  description?: string;
  children: ReactNode;
}) {
  return (
    <Collapsible>
      <Card>
        <CardHeader className="p-0">
          <CollapsibleTrigger
            render={
              <Button
                aria-label={title}
                variant="ghost"
                className="group h-auto w-full justify-between rounded-none px-4 py-3"
              />
            }
          >
            <span className="flex items-center gap-2">
              <InfoIcon />
              <span className="flex flex-col items-start gap-0.5 text-left">
                <span className="text-sm font-medium">{title}</span>
                <span className="text-xs font-normal text-muted-foreground">
                  {description}
                </span>
              </span>
            </span>
            <ChevronRightIcon className="transition-transform group-data-[state=open]:hidden" />
            <ChevronUpIcon className="hidden transition-transform group-data-[state=open]:block" />
          </CollapsibleTrigger>
        </CardHeader>
        <CollapsibleContent>
          <CardContent className="border-t">{children}</CardContent>
        </CollapsibleContent>
      </Card>
    </Collapsible>
  );
}
