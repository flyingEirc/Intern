"use client";

import { ShieldCheckIcon } from "lucide-react";
import Link from "next/link";
import { useState } from "react";
import { Diagnostics } from "@/components/resources/diagnostics";
import { ReplicaStatusBadge } from "@/components/resources/replica-status";
import { ResourceError } from "@/components/resources/states";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import { formatAge, formatBytes, formatDateTime } from "@/lib/resources/format";
import {
  useRepositoryChoices,
  useSourceChains,
  useSourceProtection,
} from "@/lib/resources/queries";
import { backupKindText, backupStatusText } from "@/lib/resources/runs";

/** Link to the restore wizard for one completed backup, when the session may plan. */
export function RestorePointLink({ backupId }: { backupId: string }) {
  return (
    <Button
      variant="outline"
      size="sm"
      nativeButton={false}
      render={<Link href={`/restores/new?backup_id=${backupId}`} />}
    >
      恢复到此备份
    </Button>
  );
}

/**
 * One source's chains and its protection points. The local point and every
 * remote point are separate rows: a remote point that lags is shown as lagging,
 * never folded into the local one.
 */
export function SourceProtection({
  sourceId,
  canPlanRestore = false,
}: {
  sourceId: string;
  canPlanRestore?: boolean;
}) {
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
                <Badge variant={chain.pinned ? "secondary" : "outline"}>
                  {chain.pinned ? "已固定" : "未固定"}
                </Badge>
                <span className="text-muted-foreground">
                  {chain.members.length} 个备份点 · 开始{" "}
                  {formatDateTime(chain.started_at)}
                </span>
              </div>
              <Diagnostics
                title="链诊断"
                description="仅在排查链身份或服务器世代时展开。"
              >
                <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
                  <dt className="text-muted-foreground">链标识</dt>
                  <dd className="font-mono text-xs break-all">{chain.id}</dd>
                  <dt className="text-muted-foreground">服务器标识</dt>
                  <dd className="font-mono text-xs break-all">
                    {chain.server_uuid || "—"}
                  </dd>
                  <dt className="text-muted-foreground">封存时间</dt>
                  <dd>{formatDateTime(chain.sealed_at)}</dd>
                </dl>
              </Diagnostics>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>序号</TableHead>
                    <TableHead>类型</TableHead>
                    <TableHead>状态</TableHead>
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
                      <TableCell>{formatBytes(member.payload_size)}</TableCell>
                      <TableCell>
                        {member.local_present ? "存在" : "已不在"}
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-wrap items-center gap-2">
                          <Link
                            className="underline-offset-4 hover:underline"
                            href={`/backups/detail?id=${member.id}`}
                          >
                            查看副本状态
                          </Link>
                          {member.replicas.map((replica) => (
                            <ReplicaStatusBadge
                              key={replica.repository_id}
                              status={replica.status}
                            />
                          ))}
                          {canPlanRestore && member.status === "completed" ? (
                            <RestorePointLink backupId={member.id} />
                          ) : null}
                        </div>
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
