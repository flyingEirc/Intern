import { Badge } from "@/components/ui/badge";
import type { ReplicaStatus } from "@/lib/api/generated/types.gen";
import { replicaStatusLabel } from "@/lib/resources/runs";

const variant: Record<ReplicaStatus, "outline" | "secondary" | "destructive"> =
  {
    pending: "outline",
    uploading: "outline",
    uploaded: "secondary",
    verified: "secondary",
    failed: "destructive",
    exhausted: "destructive",
  };

export function ReplicaStatusBadge({ status }: { status: ReplicaStatus }) {
  return <Badge variant={variant[status]}>{replicaStatusLabel[status]}</Badge>;
}
