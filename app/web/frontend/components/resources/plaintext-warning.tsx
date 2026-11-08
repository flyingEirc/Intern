"use client";

import { TriangleAlertIcon } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";

const warningText =
  "该仓库使用明文 FTP：登录凭据与备份数据都会以未加密的形式在网络上传输，任何能观察链路的人都能读取。请只在受信网络中这样使用，并优先选择显式或隐式 FTPS。";

/** Persistent destructive warning shown while plain FTP is selected. */
export function PlaintextWarning() {
  return (
    <Alert variant="destructive">
      <TriangleAlertIcon />
      <AlertTitle>凭据与数据明文传输</AlertTitle>
      <AlertDescription>{warningText}</AlertDescription>
    </Alert>
  );
}

/** The same warning, compact, for list rows and detail pages. */
export function PlaintextBadge() {
  return (
    <Badge variant="destructive" title={warningText}>
      凭据明文传输
    </Badge>
  );
}
