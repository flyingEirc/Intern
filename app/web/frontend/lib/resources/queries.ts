import {
  keepPreviousData,
  type QueryClient,
  useInfiniteQuery,
  useQuery,
} from "@tanstack/react-query";
import { client } from "@/lib/api/client";
import {
  getBackupOptions,
  getBackupQueryKey,
  getPolicyQueryKey,
  getRepositoryQueryKey,
  getRestoreOptions,
  getRestoreQueryKey,
  getRunOptions,
  getRunQueryKey,
  getSourceOptions,
  getSourceProtectionOptions,
  getSourceQueryKey,
  getUserOptions,
  getUserQueryKey,
  listAuditLogsInfiniteOptions,
  listPoliciesInfiniteOptions,
  listPoliciesOptions,
  listPoliciesQueryKey,
  listReplicasOptions,
  listReplicasQueryKey,
  listRepositoriesInfiniteOptions,
  listRepositoriesOptions,
  listRepositoriesQueryKey,
  listRestoreLogsQueryKey,
  listRestoresInfiniteOptions,
  listRestoresQueryKey,
  listRunLogsQueryKey,
  listRunsInfiniteOptions,
  listRunsQueryKey,
  listSourceChainsOptions,
  listSourcesInfiniteOptions,
  listSourcesOptions,
  listSourcesQueryKey,
  listUsersOptions,
  listUsersQueryKey,
  previewPolicyScheduleOptions,
} from "@/lib/api/generated/@tanstack/react-query.gen";
import { listRestoreLogs, listRunLogs } from "@/lib/api/generated/sdk.gen";
import type {
  ReplicaStatus,
  RestoreLog,
  RestoreStatus,
  RunLog,
  RunStatus,
} from "@/lib/api/generated/types.gen";
import { auditIdNumber } from "@/lib/resources/audit";
import { isTerminalRestore } from "@/lib/resources/restore";
import { sequenceNumber } from "@/lib/runs/events";

const pageSize = 50;
/** Selects need the full choice set in one page; 200 is the contract maximum. */
const choiceLimit = 200;

/**
 * The generated infinite options leave `initialPageParam` to the caller; the
 * first page has no cursor, and the SDK omits an undefined `before`.
 */
export function useRepositoryPages() {
  return useInfiniteQuery({
    ...listRepositoriesInfiniteOptions({ query: { limit: pageSize } }),
    initialPageParam: undefined as unknown as string,
    getNextPageParam: (last) => last.next_before ?? undefined,
  });
}

export function useSourcePages() {
  return useInfiniteQuery({
    ...listSourcesInfiniteOptions({ query: { limit: pageSize } }),
    initialPageParam: undefined as unknown as string,
    getNextPageParam: (last) => last.next_before ?? undefined,
  });
}

export function usePolicyPages() {
  return useInfiniteQuery({
    ...listPoliciesInfiniteOptions({ query: { limit: pageSize } }),
    initialPageParam: undefined as unknown as string,
    getNextPageParam: (last) => last.next_before ?? undefined,
  });
}

export function useRepositoryChoices() {
  return useQuery(listRepositoriesOptions({ query: { limit: choiceLimit } }));
}

export function useSourceChoices() {
  return useQuery(listSourcesOptions({ query: { limit: choiceLimit } }));
}

export function invalidateRepositories(client: QueryClient, id?: string) {
  client.invalidateQueries({ queryKey: listRepositoriesQueryKey() });
  if (id !== undefined) {
    client.invalidateQueries({
      queryKey: getRepositoryQueryKey({ path: { id } }),
    });
  }
}

export function invalidateSources(client: QueryClient, id?: string) {
  client.invalidateQueries({ queryKey: listSourcesQueryKey() });
  if (id !== undefined) {
    client.invalidateQueries({ queryKey: getSourceQueryKey({ path: { id } }) });
  }
}

export function invalidatePolicies(client: QueryClient, id?: string) {
  client.invalidateQueries({ queryKey: listPoliciesQueryKey() });
  if (id !== undefined) {
    client.invalidateQueries({ queryKey: getPolicyQueryKey({ path: { id } }) });
  }
}

const logPageSize = 200;

export type RunListFilters = {
  status?: RunStatus;
  policyId?: string;
};

/** Newest first, one cursor page at a time; filters are exact matches. */
export function useRunPages(filters: RunListFilters) {
  return useInfiniteQuery({
    ...listRunsInfiniteOptions({
      query: {
        limit: pageSize,
        status: filters.status,
        policy_id: filters.policyId === "" ? undefined : filters.policyId,
      },
    }),
    initialPageParam: undefined as unknown as string,
    getNextPageParam: (last) => last.next_before ?? undefined,
  });
}

export function useRun(id: string | null) {
  return useQuery({
    ...getRunOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
}

export type RunLogView = {
  items: RunLog[];
  /** True once the server's per-run cap was reached and the log ends there. */
  truncated: boolean;
};

/**
 * The bounded log, walked with `next_after` until the server reports the end
 * of what it has. One query, not an infinite one: the cap is 1000 rows, and
 * the page has to hold everything it can before it subscribes to the live
 * stream, whose resume point is the last row seen here.
 */
export function useRunLog(id: string | null) {
  return useQuery({
    queryKey: listRunLogsQueryKey({
      path: { id: id ?? "" },
      query: { limit: logPageSize },
    }),
    enabled: id !== null,
    queryFn: async ({ signal }): Promise<RunLogView> => {
      const items: RunLog[] = [];
      let afterSeq = 0;
      let truncated = false;
      for (;;) {
        const { data } = await listRunLogs({
          client,
          path: { id: id ?? "" },
          query: { limit: logPageSize, after_seq: afterSeq },
          signal,
          throwOnError: true,
        });
        items.push(
          ...data.items.map((item) => ({
            ...item,
            seq: sequenceNumber(item.seq),
          })),
        );
        truncated = data.truncated;
        if (data.next_after === null || data.items.length === 0) {
          return { items, truncated };
        }
        afterSeq = sequenceNumber(data.next_after);
      }
    },
  });
}

export function invalidateRun(client: QueryClient, id: string) {
  client.invalidateQueries({ queryKey: listRunsQueryKey() });
  client.invalidateQueries({ queryKey: getRunQueryKey({ path: { id } }) });
  client.invalidateQueries({
    queryKey: listRunLogsQueryKey({
      path: { id },
      query: { limit: logPageSize },
    }),
  });
}

export function invalidateReplicas(client: QueryClient) {
  client.invalidateQueries({ queryKey: listReplicasQueryKey() });
}

export function invalidateBackup(client: QueryClient, id: string) {
  client.invalidateQueries({ queryKey: getBackupQueryKey({ path: { id } }) });
}

/** How many upcoming fires the schedule preview card asks for. */
export const schedulePreviewCount = 10;

export function usePolicySchedule(id: string | null) {
  return useQuery({
    ...previewPolicyScheduleOptions({
      path: { id: id ?? "" },
      query: { count: schedulePreviewCount },
    }),
    enabled: id !== null,
  });
}

export function usePolicyChoices() {
  return useQuery(listPoliciesOptions({ query: { limit: choiceLimit } }));
}

export function useSourceChains(id: string | null) {
  return useQuery({
    ...listSourceChainsOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
}

export function useSourceProtection(id: string | null) {
  return useQuery({
    ...getSourceProtectionOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
}

export function useSourceName(id: string | null) {
  return useQuery({
    ...getSourceOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
    select: (source) => source.name,
  });
}

export function useBackup(id: string | null) {
  return useQuery({
    ...getBackupOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
}

export type ReplicaListFilters = {
  status?: ReplicaStatus;
  repositoryId?: string;
};

/**
 * The replica queue. The contract offers no cursor, only a bounded FIFO page,
 * so the page size is the contract maximum and the view says so.
 */
export function useReplicaQueue(filters: ReplicaListFilters) {
  return useQuery(
    listReplicasOptions({
      query: {
        limit: choiceLimit,
        status: filters.status,
        repository_id:
          filters.repositoryId === "" ? undefined : filters.repositoryId,
      },
    }),
  );
}

export type RestoreListFilters = {
  status?: RestoreStatus;
};

/** Newest first, one cursor page at a time; the status filter is an exact match. */
export function useRestorePages(filters: RestoreListFilters) {
  return useInfiniteQuery({
    ...listRestoresInfiniteOptions({
      query: { limit: pageSize, status: filters.status },
    }),
    initialPageParam: undefined as unknown as string,
    getNextPageParam: (last) => last.next_before ?? undefined,
  });
}

const restorePollMs = 2000;

/**
 * One restore. The contract has no restore SSE, so a non-terminal row is
 * polled until a terminal write nulls the phase.
 */
export function useRestore(id: string | null) {
  return useQuery({
    ...getRestoreOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status !== undefined && !isTerminalRestore(status)
        ? restorePollMs
        : false;
    },
  });
}

export type RestoreLogView = {
  items: RestoreLog[];
  truncated: boolean;
};

/**
 * The bounded restore log, walked with `next_after` and polled while live.
 * `live` is undefined until the restore row is known; the key carries it, so
 * the switch to a terminal row reads the log once more and the terminal
 * entries written after the last poll are not lost.
 */
export function useRestoreLog(id: string | null, live: boolean | undefined) {
  return useQuery({
    queryKey: [
      ...listRestoreLogsQueryKey({
        path: { id: id ?? "" },
        query: { limit: logPageSize },
      }),
      { live },
    ],
    enabled: id !== null && live !== undefined,
    placeholderData: keepPreviousData,
    refetchInterval: live ? restorePollMs : false,
    queryFn: async ({ signal }): Promise<RestoreLogView> => {
      const items: RestoreLog[] = [];
      let afterSeq = 0;
      let truncated = false;
      for (;;) {
        const { data } = await listRestoreLogs({
          client,
          path: { id: id ?? "" },
          query: { limit: logPageSize, after_seq: afterSeq },
          signal,
          throwOnError: true,
        });
        items.push(
          ...data.items.map((item) => ({
            ...item,
            seq: sequenceNumber(item.seq),
          })),
        );
        truncated = data.truncated;
        if (data.next_after === null || data.items.length === 0) {
          return { items, truncated };
        }
        afterSeq = sequenceNumber(data.next_after);
      }
    },
  });
}

export function invalidateRestore(client: QueryClient, id: string) {
  client.invalidateQueries({ queryKey: listRestoresQueryKey() });
  client.invalidateQueries({ queryKey: getRestoreQueryKey({ path: { id } }) });
  client.invalidateQueries({
    queryKey: listRestoreLogsQueryKey({
      path: { id },
      query: { limit: logPageSize },
    }),
  });
}

export function useUsers(enabled: boolean) {
  return useQuery({ ...listUsersOptions(), enabled });
}

export function useUser(id: string | null) {
  return useQuery({
    ...getUserOptions({ path: { id: id ?? "" } }),
    enabled: id !== null,
  });
}

/**
 * Newest audit rows first. The first page has no cursor; `next_before` is the
 * last id on the page, narrowed to the number the generated query accepts.
 */
export function useAuditLogPages(enabled: boolean) {
  return useInfiniteQuery({
    ...listAuditLogsInfiniteOptions({ query: { limit: pageSize } }),
    initialPageParam: undefined as unknown as number,
    getNextPageParam: (last) =>
      last.next_before === null ? undefined : auditIdNumber(last.next_before),
    enabled,
  });
}

export function invalidateUsers(client: QueryClient, id: string) {
  client.invalidateQueries({ queryKey: listUsersQueryKey() });
  client.invalidateQueries({ queryKey: getUserQueryKey({ path: { id } }) });
}
