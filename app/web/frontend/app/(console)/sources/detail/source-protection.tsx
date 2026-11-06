"use client";

import { ShieldCheckIcon } from "lucide-react";
import Link from "next/link";
import { useState } from "react";
import { ReplicaStatusBadge } from "@/components/resources/replica-status";
import { ResourceError } from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { Point } from "@/lib/api/generated/types.gen";
import {
  formatAge,
  formatBytes,
  formatDateTime,
  formatLsn,
} from "@/lib/resources/format";
import {
  useRepositoryChoices,
  useSourceChains,
  useSourceProtection,
} from "@/lib/resources/queries";
import { backupKindText, backupStatusText } from "@/lib/resources/runs";

/**
 * One source's chains and its protection points. The local point and every
 * remote point are separate rows: a remote point that lags is shown as lagging,
 * never folded into the local one.
 */
export function SourceProtection({ sourceId }: { sourceId: string }) {
  const chains = useSourceChains(sourceId);
  const protection = useSourceProtection(sourceId);
  const repositories = useRepositoryChoices();
  const [now] = useState(() => new Date());
  const repositoryName = (id: string | undefined) =>
    id === undefined
      ? "—"
      : (repositories.data?.items.find((item) => item.id === id)?.name ?? id);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheckIcon className="size-4" />
          备份链与保护点
        </CardTitle>
        <CardDescription>
          本地保护点与每个异地仓库的保护点分开计算、分开显示，不合并成一个数字。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-6">
        <section className="flex flex-col gap-3">
          <h3 className="text-sm font-medium">保护点</h3>
          {protection.isPending ? <Skeleton className="h-16 w-full" /> : null}
          {protection.isError ? (
            <ResourceError
              title="无法读取保护点"
              error={protection.error}
              onRetry={() => protection.refetch()}
            />
          ) : null}
          {protection.isSuccess ? (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>位置</TableHead>
                  <TableHead>仓库</TableHead>
                  <TableHead>备份</TableHead>
                  <TableHead>时间</TableHead>
                  <TableHead>距今</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                <ProtectionRow
                  place="本地"
                  repository="本地仓库"
                  point={protection.data.local}
                  now={now}
                />
                {protection.data.remote.map((point) => (
                  <ProtectionRow
                    key={point.repository_id ?? point.backup_id}
                    place="异地"
                    repository={repositoryName(point.repository_id)}
                    point={point}
                    now={now}
                  />
                ))}
              </TableBody>
            </Table>
          ) : null}
          {protection.isSuccess && protection.data.remote.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              还没有异地保护点：没有任何副本仓库完成过校验。
            </p>
          ) : null}
        </section>

        <section className="flex flex-col gap-3">
          <h3 className="text-sm font-medium">备份链</h3>
          {chains.isPending ? <Skeleton className="h-16 w-full" /> : null}
          {chains.isError ? (
            <ResourceError
              title="无法读取备份链"
              error={chains.error}
              onRetry={() => chains.refetch()}
            />
          ) : null}
          {chains.isSuccess && chains.data.items.length === 0 ? (
            <p className="text-sm text-muted-foreground">还没有备份链。</p>
          ) : null}
          {chains.data?.items.map((chain) => (
            <div key={chain.id} className="flex flex-col gap-2">
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <span className="font-mono text-xs">{chain.id}</span>
                <Badge variant={chain.pinned ? "secondary" : "outline"}>
                  {chain.pinned ? "已固定" : "未固定"}
                </Badge>
                <span className="text-muted-foreground">
                  服务器 {chain.server_uuid || "—"} · 开始{" "}
                  {formatDateTime(chain.started_at)} · 封存{" "}
                  {formatDateTime(chain.sealed_at)}
                </span>
              </div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>序号</TableHead>
                    <TableHead>类型</TableHead>
                    <TableHead>状态</TableHead>
                    <TableHead>LSN</TableHead>
                    <TableHead>大小</TableHead>
                    <TableHead>本地</TableHead>
                    <TableHead>副本</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {chain.members.map((member) => (
                    <TableRow key={member.id}>
                      <TableCell>{member.seq}</TableCell>
                      <TableCell>{backupKindText(member.kind)}</TableCell>
                      <TableCell>{backupStatusText(member.status)}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {formatLsn(member.from_lsn)} →{" "}
                        {formatLsn(member.to_lsn)}
                      </TableCell>
                      <TableCell>{formatBytes(member.payload_size)}</TableCell>
                      <TableCell>
                        {member.local_present ? "存在" : "已不在"}
                      </TableCell>
                      <TableCell>
                        <Link
                          className="flex flex-wrap items-center gap-1 underline-offset-4 hover:underline"
                          href={`/backups/detail?id=${member.id}`}
                        >
                          {member.replicas.length === 0
                            ? "无副本"
                            : member.replicas.map((replica) => (
                                <ReplicaStatusBadge
                                  key={replica.repository_id}
                                  status={replica.status}
                                />
                              ))}
                        </Link>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          ))}
        </section>
      </CardContent>
    </Card>
  );
}

function ProtectionRow({
  place,
  repository,
  point,
  now,
}: {
  place: string;
  repository: string;
  point: Point | null;
  now: Date;
}) {
  const age = point ? formatAge(point.at, now) : null;
  return (
    <TableRow>
      <TableCell>{place}</TableCell>
      <TableCell>{repository}</TableCell>
      <TableCell>
        {point ? (
          <Link
            className="font-mono text-xs underline-offset-4 hover:underline"
            href={`/backups/detail?id=${point.backup_id}`}
          >
            {point.backup_id}
          </Link>
        ) : (
          "尚无保护点"
        )}
      </TableCell>
      <TableCell>{point ? formatDateTime(point.at) : "—"}</TableCell>
      <TableCell>
        {age ? (
          <span className={age.stale ? "text-destructive" : undefined}>
            {age.text}
          </span>
        ) : (
          "—"
        )}
      </TableCell>
    </TableRow>
  );
}
