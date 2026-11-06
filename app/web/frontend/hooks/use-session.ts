"use client";

import { useQuery } from "@tanstack/react-query";
import { currentSessionOptions } from "@/lib/api/generated/@tanstack/react-query.gen";

/** The signed-in user and effective permissions; 401 triggers the login redirect. */
export function useSession() {
  return useQuery(currentSessionOptions());
}
