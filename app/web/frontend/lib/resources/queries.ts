import {
  type QueryClient,
  useInfiniteQuery,
  useQuery,
} from "@tanstack/react-query";
import {
  getPolicyQueryKey,
  getRepositoryQueryKey,
  getSourceQueryKey,
  listPoliciesInfiniteOptions,
  listPoliciesQueryKey,
  listRepositoriesInfiniteOptions,
  listRepositoriesOptions,
  listRepositoriesQueryKey,
  listSourcesInfiniteOptions,
  listSourcesOptions,
  listSourcesQueryKey,
} from "@/lib/api/generated/@tanstack/react-query.gen";

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
